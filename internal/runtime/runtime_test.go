package runtime

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	rt, err := Build(cfg, discard)
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
	rt, err := Build(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if len(rt.Pools) != 2 || len(rt.Pools["api"].Backends) != 2 {
		t.Errorf("pools = %v", rt.Pools)
	}
	out := logs.String()
	for _, want := range []string{
		"balancer=least_conn",
		"option=upstreams.api.health",
		"option=upstreams.api.circuit_breaker",
		"option=routes[0].max_body",
		"option=routes[0].rate_limit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warnings %q do not mention %s", out, want)
		}
	}
	if strings.Contains(out, "routes[1]") || strings.Contains(out, "upstreams.static") {
		t.Errorf("warned about options that are not set: %q", out)
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
	_, err := Build(cfg, discard)
	if err == nil || !strings.Contains(err.Error(), "upstream secure") || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Errorf("Build error = %v, want the bad CA file named", err)
	}
}

func TestBuildUnknownUpstream(t *testing.T) {
	// Validation catches this; Build must not panic on an unvalidated config.
	cfg := &config.Config{Routes: []config.Route{{Match: config.Match{PathPrefix: "/"}, Upstream: "missing"}}}
	if _, err := Build(cfg, discard); err == nil || !strings.Contains(err.Error(), `unknown upstream "missing"`) {
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
