package runtime

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/MuditGarg007/prism/internal/cache"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/resilience"
)

// buildFrom builds cfg as a reload of prev.
func buildFrom(t *testing.T, cfg *config.Config, prev *Runtime) *Runtime {
	t.Helper()
	rt, err := Build(cfg, discard, Options{Prev: prev})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	return rt
}

// TestReloadKeepsConnections checks that a reload with an unchanged
// transport keeps its pooled connection open through Retire, that a changed
// one gets a new connection, and that a failed reload leaves the serving
// runtime's connections alone.
func TestReloadKeepsConnections(t *testing.T) {
	var conns atomic.Int64
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	s.Start()
	defer s.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := func(idle, extra string) *config.Config {
		return parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: %q }], timeouts: { idle_conn: %s } }
%s
routes:
  - { match: { path_prefix: / }, upstream: web }
`, s.URL, idle, extra), dir)
	}
	send := func(rt *Runtime, wantConns int64) {
		t.Helper()
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://a.test/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if n := conns.Load(); n != wantConns {
			t.Errorf("%d connections to the backend, want %d", n, wantConns)
		}
	}

	rt1 := buildFrom(t, cfg("90s", ""), nil)
	send(rt1, 1)
	rt2 := buildFrom(t, cfg("90s", ""), rt1)
	rt1.Retire(rt2)
	send(rt2, 1)

	rt3 := buildFrom(t, cfg("60s", ""), rt2)
	rt2.Retire(rt3)
	send(rt3, 2)

	// "zz" sorts after "web", so web's transport is carried over before
	// the bad CA fails the build.
	bad := cfg("60s", `  zz: { backends: [{ url: "https://127.0.0.1:9443", tls: { ca_file: bad.pem } }] }`)
	if _, err := Build(bad, discard, Options{Prev: rt3}); err == nil {
		t.Fatal("Build with a bad CA file succeeded")
	}
	send(rt3, 2)
}

// TestReloadKeepsBackendState checks state carried through Build: inflight
// and health, and a breaker that stays open.
func TestReloadKeepsBackendState(t *testing.T) {
	const yaml = `
listeners: [{ addr: ":8080" }]
upstreams:
  web:
    backends: [{ url: "http://127.0.0.1:9000" }, { url: "http://127.0.0.1:9001", weight: %d }]
    health: { path: /healthz }
    circuit_breaker: { min_requests: 1 }
routes:
  - { match: { path_prefix: / }, upstream: web }
`
	rt1 := buildFrom(t, parse(t, fmt.Sprintf(yaml, 1), ""), nil)
	a, b := rt1.Pools["web"].Backends[0], rt1.Pools["web"].Backends[1]
	a.Acquire()
	defer a.Release()
	adm, _ := a.Admit()
	a.Report(adm, resilience.Failure)
	b.SetHealthy(false)

	rt2 := buildFrom(t, parse(t, fmt.Sprintf(yaml, 4), ""), rt1)
	na, nb := rt2.Pools["web"].Backends[0], rt2.Pools["web"].Backends[1]
	if na.Inflight() != 1 || na.Available() || nb.Healthy() || nb.Weight != 4 {
		t.Errorf("after reload: inflight %d, a available %v, b healthy %v, b weight %d",
			na.Inflight(), na.Available(), nb.Healthy(), nb.Weight)
	}
}

// TestReloadKeepsRouteState checks which per-route state a reload keeps,
// matched by route name, and that the cache is always kept.
// testHash is a bcrypt hash of "s3cret" at the lowest cost.
const testHash = "$2a$04$2Waq0k1CQGjrchXQb2AjfOg3u7OIuziqivVBEfq8R8TWIsZG3IYrC"

func TestReloadKeepsRouteState(t *testing.T) {
	type knobs struct {
		host, rate, budget, maxSize, realm string
		conc, queue, shadowConc            int
	}
	base := knobs{host: "a.test", rate: "10/s", budget: "0.2", maxSize: "1MB", realm: "api", conc: 4, queue: 2, shadowConc: 8}
	cfg := func(k knobs) *config.Config {
		return parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
cache: { max_size: %s }
upstreams:
  web: { backends: [{ url: "http://127.0.0.1:9000" }] }
  v2: { backends: [{ url: "http://127.0.0.1:9001" }] }
routes:
  - match: { host: %s, path_prefix: /api/ }
    upstream: web
    rate_limit: { per_ip: %s }
    max_concurrency: %d
    queue_size: %d
    retry: { attempts: 1, budget_ratio: %s }
    cache: { enabled: true }
    auth: { basic: { realm: %s, users: { alice: "%s" } } }
    shadow: { upstream: v2, max_concurrency: %d }
`, k.maxSize, k.host, k.rate, k.conc, k.queue, k.budget, k.realm, testHash, k.shadowConc), "")
	}
	tests := []struct {
		name                     string
		change                   func(k *knobs)
		limiter, sem, budget, rt bool // kept; rt is whether the route is matched at all
		auth, shadowSlots        bool
	}{
		{"unchanged", func(k *knobs) {}, true, true, true, true, true, true},
		{"rate changed", func(k *knobs) { k.rate = "20/s" }, false, true, true, true, true, true},
		{"concurrency changed", func(k *knobs) { k.conc = 8 }, true, false, true, true, true, true},
		{"queue changed", func(k *knobs) { k.queue = 3 }, true, false, true, true, true, true},
		{"budget changed", func(k *knobs) { k.budget = "0.5" }, true, true, false, true, true, true},
		{"cache resized", func(k *knobs) { k.maxSize = "2MB" }, true, true, true, true, true, true},
		{"realm changed", func(k *knobs) { k.realm = "other" }, true, true, true, true, false, true},
		{"shadow cap changed", func(k *knobs) { k.shadowConc = 9 }, true, true, true, true, true, false},
		{"renamed", func(k *knobs) { k.host = "b.test" }, false, false, false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt1 := buildFrom(t, cfg(base), nil)
			k := base
			tt.change(&k)
			rt2 := buildFrom(t, cfg(k), rt1)
			r1, r2 := rt1.Routes[0], rt2.Routes[0]
			if got := r1.RateLimit == r2.RateLimit; got != tt.limiter {
				t.Errorf("limiter kept = %v, want %v", got, tt.limiter)
			}
			if got := r1.Concurrency == r2.Concurrency; got != tt.sem {
				t.Errorf("semaphore kept = %v, want %v", got, tt.sem)
			}
			if got := r1.Shadow.Slots == r2.Shadow.Slots; got != tt.shadowSlots {
				t.Errorf("shadow slots kept = %v, want %v", got, tt.shadowSlots)
			}
			if got := r1.Auth == r2.Auth; got != tt.auth {
				t.Errorf("authenticator kept = %v, want %v", got, tt.auth)
			}
			if got := r1.Retry.Budget == r2.Retry.Budget; got != tt.budget {
				t.Errorf("budget kept = %v, want %v", got, tt.budget)
			}
			if r1.Retry == r2.Retry {
				t.Error("the Retry itself is shared; only its budget should be")
			}
			c1, c2 := rt1.Cache, rt2.Cache
			if c2.Store != c1.Store || c2.Flight != c1.Flight || c2.Stats != c1.Stats || r2.Cache.Store != c1.Store {
				t.Error("the cache was not carried over")
			}
			if want := int64(cfg(k).Cache.MaxSize); c2.MaxSize != want || c1.MaxSize != int64(cfg(base).Cache.MaxSize) {
				t.Errorf("MaxSize = %d (old %d), want %d without changing the old runtime's", c2.MaxSize, c1.MaxSize, want)
			}
		})
	}
}

// TestReloadShrinksCache checks that a smaller max_size evicts entries
// from the carried-over store, and that removing every caching route drops
// the cache.
func TestReloadShrinksCache(t *testing.T) {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Write(make([]byte, 100<<10))
	}))
	defer b.Close()
	cfg := func(size string, enabled bool) *config.Config {
		return parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
cache: { max_size: %s }
upstreams:
  web: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: web, cache: { enabled: %t, max_object: 200KB } }
`, size, b.URL, enabled), "")
	}
	rt1 := buildFrom(t, cfg("1MB", true), nil)
	for i := range 5 {
		rt1.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", fmt.Sprintf("http://a.test/%d", i), nil))
	}
	if n := rt1.Cache.Store.Stats().Entries; n != 5 {
		t.Fatalf("%d entries stored, want 5", n)
	}
	rt2 := buildFrom(t, cfg("250KB", true), rt1)
	if st := rt2.Cache.Store.Stats(); st.Entries != 2 || st.Evictions != 3 {
		t.Errorf("after shrinking: %+v, want 2 entries left", st)
	}
	rec := httptest.NewRecorder()
	rt2.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://a.test/4", nil))
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("most recent entry: X-Cache %q, want HIT", got)
	}
	if rt3 := buildFrom(t, cfg("250KB", false), rt2); rt3.Cache != nil {
		t.Error("a runtime with no caching route kept the cache")
	}
}

// TestFailedReloadKeepsCacheSize checks that a reload that fails leaves the
// serving runtime's cache at its size.
func TestFailedReloadKeepsCacheSize(t *testing.T) {
	cfg := func(size string) *config.Config {
		return parse(t, fmt.Sprintf(`
listeners: [{ addr: ":8080" }]
cache: { max_size: %s }
upstreams:
  web: { backends: [{ url: "http://127.0.0.1:9000" }] }
routes:
  - { match: { path_prefix: / }, upstream: web, cache: { enabled: true, max_object: 1KB } }
`, size), "")
	}
	rt1 := buildFrom(t, cfg("1MB"), nil)
	for i := range 10 {
		rt1.Cache.Store.Put(cache.NewEntry(fmt.Sprintf("GET http://a.test/%d", i), http.Header{}, 200, http.Header{}, make([]byte, 1000), cache.Directives{}, cache.Freshness{}))
	}
	bad := cfg("2KB")
	bad.Routes = append(bad.Routes, config.Route{Match: config.Match{PathPrefix: "/x/"}, Upstream: "missing"})
	if _, err := Build(bad, discard, Options{Prev: rt1}); err == nil {
		t.Fatal("Build with an unknown upstream succeeded")
	}
	if n := rt1.Cache.Store.Stats().Entries; n != 10 {
		t.Errorf("%d entries left after a failed reload, want 10", n)
	}
}
