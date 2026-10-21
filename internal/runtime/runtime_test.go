package runtime

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
)

var discard = slog.New(slog.DiscardHandler)

// namedBackend answers with its name, the request URI it got, and its
// X-Env header.
func namedBackend(t *testing.T, name string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s %s", name, r.RequestURI, r.Header.Get("X-Env"))
	}))
	t.Cleanup(s.Close)
	return s
}

func parse(t *testing.T, yaml, baseDir string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml), baseDir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func build(t testing.TB, cfg *config.Config) *Runtime {
	t.Helper()
	rt, err := Build(cfg, discard, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	return rt
}

// singleUpstream is a config with every request going to url.
func singleUpstream(t *testing.T, url string) *config.Config {
	return parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: "/" }, upstream: web }
`, url), "")
}

func TestBuildExample(t *testing.T) {
	cfg, err := config.Load("../../examples/prism.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	rt, err := Build(cfg, slog.New(slog.NewTextHandler(&logs, nil)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if len(rt.Pools) != 3 || len(rt.Pools["api"].Backends) != 2 {
		t.Errorf("pools = %v", rt.Pools)
	}
	// The example's admin API is on loopback, and everything it sets is
	// implemented, so Build has nothing to warn about.
	if out := logs.String(); out != "" {
		t.Errorf("warnings: %q", out)
	}
	if rt.Cache == nil || rt.Cache.MaxSize != 64<<20 {
		t.Errorf("cache = %+v, want a 64 MB store", rt.Cache)
	}
}

func TestBuildBasicAuthWarning(t *testing.T) {
	for _, rl := range []string{"", ", rate_limit: { per_ip: 10/s }"} {
		cfg := parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: "http://127.0.0.1:1" }] }
routes:
  - { match: { path_prefix: / }, upstream: web, auth: { basic: { users: { alice: %q } } }%s }
`, testHash, rl), "")
		var logs bytes.Buffer
		rt, err := Build(cfg, slog.New(slog.NewTextHandler(&logs, nil)), Options{})
		if err != nil {
			t.Fatal(err)
		}
		rt.Close()
		if warned := strings.Contains(logs.String(), "no rate_limit"); warned != (rl == "") {
			t.Errorf("rate limit %q: warned %v; logs %q", rl, warned, logs.String())
		}
	}
}

func TestBuildAdminWarning(t *testing.T) {
	for _, tt := range []struct {
		listen string
		warn   bool
	}{
		{"127.0.0.1:9901", false},
		{"[::1]:9901", false},
		{"localhost:9901", false},
		{":9901", true},
		{"0.0.0.0:9901", true},
		{"10.0.0.1:9901", true},
	} {
		cfg := singleUpstream(t, "http://127.0.0.1:9000")
		cfg.Admin = &config.Admin{Listen: tt.listen}
		var logs bytes.Buffer
		rt, err := Build(cfg, slog.New(slog.NewTextHandler(&logs, nil)), Options{})
		if err != nil {
			t.Fatal(err)
		}
		rt.Close()
		if got := strings.Contains(logs.String(), "beyond loopback"); got != tt.warn {
			t.Errorf("%s: warned %v, want %v: %q", tt.listen, got, tt.warn, logs.String())
		}
	}
}

// TestBuildCache checks that only routes with caching enabled cache, that
// they share one store, and that bypass_header matches whatever its case.
func TestBuildCache(t *testing.T) {
	var hits atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, r.URL.Path)
	}))
	defer s.Close()
	rt := build(t, parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
cache: { max_size: 1MB }
upstreams:
  web: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: /a/ }, upstream: web, cache: { enabled: true, bypass_header: x-no-cache } }
  - { match: { path_prefix: /b/ }, upstream: web, cache: { enabled: true } }
  - { match: { path_prefix: /off/ }, upstream: web, cache: { enabled: false } }
  - { match: { path_prefix: / }, upstream: web }
`, s.URL), ""))
	if rt.Cache == nil || rt.Cache.MaxSize != 1<<20 {
		t.Fatalf("cache = %+v", rt.Cache)
	}

	serve := func(path string, h http.Header) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		maps.Copy(req.Header, h)
		rt.Handler.ServeHTTP(rec, req)
		return rec.Header().Get("X-Cache")
	}
	tests := []struct {
		path   string
		header http.Header
		want   []string // X-Cache of two requests in a row
		hits   int64    // backend requests they make
	}{
		{"/a/x", nil, []string{"MISS", "HIT"}, 1},
		{"/b/x", nil, []string{"MISS", "HIT"}, 1},
		{"/a/x", http.Header{"X-No-Cache": {"1"}}, []string{"BYPASS", "BYPASS"}, 2},
		{"/off/x", nil, []string{"", ""}, 2},
		{"/x", nil, []string{"", ""}, 2},
	}
	for _, tt := range tests {
		before := hits.Load()
		for i, want := range tt.want {
			if got := serve(tt.path, tt.header); got != want {
				t.Errorf("%s %v request %d: X-Cache %q, want %q", tt.path, tt.header, i, got, want)
			}
		}
		if n := hits.Load() - before; n != tt.hits {
			t.Errorf("%s %v: %d backend requests, want %d", tt.path, tt.header, n, tt.hits)
		}
	}
	if st := rt.Cache.Store.Stats(); st.Entries != 2 {
		t.Errorf("store holds %d entries, want 2", st.Entries)
	}
	if n := rt.Cache.Stats.Snapshot(); n.Hit != 2 || n.Miss != 2 || n.Bypass != 2 {
		t.Errorf("stats = %+v", n)
	}
}

func TestBuildNoCache(t *testing.T) {
	cfg := parse(t, `
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: "http://127.0.0.1:9000" }] }
routes:
  - { match: { path_prefix: / }, upstream: web, cache: { enabled: false } }
`, "")
	if rt := build(t, cfg); rt.Cache != nil {
		t.Errorf("built a cache no route uses: %+v", rt.Cache)
	}
}

func TestBuildRoutes(t *testing.T) {
	a, b := namedBackend(t, "a"), namedBackend(t, "b")
	cfg := parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
upstreams:
  a: { backends: [{ url: %q }] }
  b: { backends: [{ url: %q }] }
routes:
  - match: { host: api.test, path_prefix: /v1/ }
    upstream: a
    strip_prefix: true
    headers: { request_set: { X-Env: prod }, response_set: { X-Via: prism } }
  - { match: { path_prefix: / }, upstream: b }
`, a.URL, b.URL), "")
	front := httptest.NewServer(NewCurrent(build(t, cfg)))
	defer front.Close()

	tests := []struct {
		host, path string
		status     int
		body, via  string
	}{
		{"api.test", "/v1/users?id=1", 200, "a /users?id=1 prod", "prism"},
		{"API.test:443", "/v1/", 200, "a / prod", "prism"},
		{"other.test", "/v1/users", 200, "b /v1/users ", ""},
		// api.test has its own host group, so there is no fallback to "/".
		{"api.test", "/other", 404, "404 page not found\n", ""},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest("GET", front.URL+tt.path, nil)
		req.Host = tt.host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tt.status || string(body) != tt.body || resp.Header.Get("X-Via") != tt.via {
			t.Errorf("%s%s: got %d %q via %q, want %d %q via %q",
				tt.host, tt.path, resp.StatusCode, body, resp.Header.Get("X-Via"), tt.status, tt.body, tt.via)
		}
	}
}

func TestBuildBadCA(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := parse(t, `
listeners: [{ addr: ":8080" }]
upstreams:
  ok: { backends: [{ url: "http://127.0.0.1:9000" }] }
  secure: { backends: [{ url: "https://127.0.0.1:9443", tls: { ca_file: ca.pem } }] }
routes:
  - { match: { path_prefix: / }, upstream: secure }
`, dir)
	_, err := Build(cfg, discard, Options{})
	if err == nil || !strings.Contains(err.Error(), "upstream secure") || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Errorf("Build error = %v, want the bad CA file named", err)
	}
}

func TestBuildUnknownUpstream(t *testing.T) {
	// Validation catches this; Build must not panic on an unvalidated config.
	cfg := &config.Config{Routes: []config.Route{{Match: config.Match{PathPrefix: "/"}, Upstream: "missing"}}}
	if _, err := Build(cfg, discard, Options{}); err == nil || !strings.Contains(err.Error(), `unknown upstream "missing"`) {
		t.Errorf("Build error = %v", err)
	}
}

func TestCurrentSwap(t *testing.T) {
	a, b := namedBackend(t, "a"), namedBackend(t, "b")
	rtA, rtB := build(t, singleUpstream(t, a.URL)), build(t, singleUpstream(t, b.URL))
	cur := NewCurrent(rtA)
	front := httptest.NewServer(cur)
	defer front.Close()

	get := func() string {
		resp, err := http.Get(front.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	if got := get(); got != "a /x " {
		t.Errorf("before swap: %q", got)
	}
	if old := cur.Swap(rtB); old != rtA {
		t.Error("Swap did not return the previous runtime")
	}
	if cur.Load() != rtB {
		t.Error("Load does not return the swapped-in runtime")
	}
	if got := get(); got != "b /x " {
		t.Errorf("after swap: %q", got)
	}
}

// TestCurrentSwapRace serves requests while swapping runtimes, for the race
// detector. The requests miss every route, so no backend is needed.
func TestCurrentSwapRace(t *testing.T) {
	rts := []*Runtime{
		build(t, singleUpstreamAt(t, "/a/")),
		build(t, singleUpstreamAt(t, "/b/")),
	}
	cur := NewCurrent(rts[0])
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				rec := httptest.NewRecorder()
				cur.ServeHTTP(rec, httptest.NewRequest("GET", "/miss", nil))
				if rec.Code != http.StatusNotFound {
					t.Errorf("status %d", rec.Code)
					return
				}
			}
		})
	}
	for i := range 100 {
		cur.Swap(rts[i%2])
	}
	wg.Wait()
}

func singleUpstreamAt(t *testing.T, prefix string) *config.Config {
	return parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: "http://127.0.0.1:9000" }] }
routes:
  - { match: { path_prefix: %q }, upstream: web }
`, prefix), "")
}

type nopWriter struct{ h http.Header }

func (w *nopWriter) Header() http.Header         { return w.h }
func (w *nopWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *nopWriter) WriteHeader(int)             {}

// BenchmarkCurrent measures a request that misses every route, which is the
// cost of the snapshot load, path check and route match without any
// backend.
func BenchmarkCurrent(b *testing.B) {
	cfg, err := config.Parse([]byte(`
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: "http://127.0.0.1:9000" }] }
routes:
  - { match: { path_prefix: /api/ }, upstream: web }
`), "")
	if err != nil {
		b.Fatal(err)
	}
	cur := NewCurrent(build(b, cfg))
	w := &nopWriter{h: http.Header{}}
	r := httptest.NewRequest("GET", "/miss", nil)
	b.ReportAllocs()
	for b.Loop() {
		cur.ServeHTTP(w, r)
	}
}

// TestStartHealth checks that Build sends nothing to backends, so -validate
// stays quiet, and that Start begins health checks which mark a failing
// backend down.
func TestStartHealth(t *testing.T) {
	var checks atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer s.Close()
	rt := build(t, parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
upstreams:
  web:
    backends: [{ url: %q }]
    health: { path: /healthz, interval: 2ms, timeout: 1ms, unhealthy_after: 1 }
routes:
  - { match: { path_prefix: "/" }, upstream: web }
`, s.URL), ""))
	time.Sleep(20 * time.Millisecond)
	if n := checks.Load(); n != 0 {
		t.Fatalf("%d checks before Start", n)
	}
	rt.Start()
	b := rt.Pools["web"].Backends[0]
	for deadline := time.Now().Add(5 * time.Second); b.Available(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("backend never marked down")
		}
	}
	rt.Close()
}

// TestBuildSecurityAndSplits checks that Build wires access lists, auth,
// the canary and the shadow into the handler.
func TestBuildSecurityAndSplits(t *testing.T) {
	backend := func(name string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, name+" "+r.Header.Get("X-Auth-User"))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	cfg := parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
access: { deny: [192.0.2.0/24] }
upstreams:
  web: { backends: [{ url: %q }] }
  v2: { backends: [{ url: %q }] }
routes:
  - match: { path_prefix: / }
    upstream: web
    access: { allow: [10.0.0.0/8] }
    auth: { basic: { users: { alice: %q } }, identity_header: X-Auth-User }
    canary: { upstream: v2, percent: 2.5, header: x-canary }
    shadow: { upstream: v2, percent: 50, max_concurrency: 3 }
    websocket: { idle_timeout: 90s }
`, backend("web"), backend("v2"), testHash), "")
	rt := build(t, cfg)
	r := rt.Routes[0]
	if c := r.Canary; c == nil || c.Pool != rt.Pools["v2"] || c.Buckets != 250 || c.Header != "X-Canary" {
		t.Fatalf("canary = %+v", r.Canary)
	}
	if s := r.Shadow; s == nil || s.Pool != rt.Pools["v2"] || s.Buckets != 5000 || s.Slots.Cap() != 3 || s.MaxBody != 64<<10 {
		t.Fatalf("shadow = %+v", r.Shadow)
	}
	if r.WebSocketIdle != 90*time.Second {
		t.Errorf("WebSocketIdle = %v, want 90s", r.WebSocketIdle)
	}

	send := func(addr string, hdr ...string) (int, string) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = addr + ":1234"
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		rt.Handler.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if code, _ := send("192.0.2.1"); code != http.StatusForbidden {
		t.Errorf("globally denied client got %d, want 403", code)
	}
	if code, _ := send("198.51.100.1"); code != http.StatusForbidden {
		t.Errorf("client outside the route's allow list got %d, want 403", code)
	}
	if code, _ := send("10.0.0.1"); code != http.StatusUnauthorized {
		t.Errorf("client without credentials got %d, want 401", code)
	}
	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if code, body := send("10.0.0.1", "Authorization", creds, "X-Canary", "always"); code != 200 || body != "v2 alice" {
		t.Errorf("canary request got %d %q, want 200 \"v2 alice\"", code, body)
	}
	if code, body := send("10.0.0.1", "Authorization", creds, "X-Canary", "never"); code != 200 || body != "web alice" {
		t.Errorf("primary request got %d %q, want 200 \"web alice\"", code, body)
	}
}
