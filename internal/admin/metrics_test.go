package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/runtime"
)

// metricsRuntime builds a runtime with every kind of state the collector
// reports: two backends with a breaker and outlier detection, a route with
// a concurrency limit, and a caching route.
func metricsRuntime(t *testing.T, m *observability.Metrics, backends ...string) *runtime.Runtime {
	t.Helper()
	var list []string
	for _, b := range backends {
		list = append(list, fmt.Sprintf("{ url: %q }", b))
	}
	cfg, err := config.Parse(fmt.Appendf(nil, `
listeners: [{ addr: ":8080" }]
cache: { max_size: 1MB }
upstreams:
  web:
    backends: [%s]
    circuit_breaker: { failure_ratio: 0.5, min_requests: 20, open_for: 30s }
    outlier: { consecutive_failures: 5, base_ejection: 30s, max_ejection: 300s, max_ejection_percent: 50 }
routes:
  - { match: { path_prefix: /api/ }, upstream: web, max_concurrency: 8, queue_size: 4, queue_timeout: 1s }
  - { match: { path_prefix: / }, upstream: web, cache: { enabled: true } }
`, strings.Join(list, ", ")), "")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.Build(cfg, discard, runtime.Options{Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	return rt
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := do(h, "GET", "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestMetrics(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
	}))
	defer backend.Close()
	const down = "http://127.0.0.1:1"
	m := observability.NewMetrics()
	rt := metricsRuntime(t, m, backend.URL, down)
	h := New(runtime.NewCurrent(rt), m, nil, discard)

	rt.Pools["web"].Backends[1].SetHealthy(false)
	for range 2 {
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/page", nil))
	}

	out := scrape(t, h)
	for _, want := range []string{
		fmt.Sprintf(`prism_backend_inflight{backend=%q,upstream="web"} 0`, backend.URL),
		fmt.Sprintf(`prism_backend_healthy{backend=%q,upstream="web"} 1`, backend.URL),
		fmt.Sprintf(`prism_backend_healthy{backend=%q,upstream="web"} 0`, down),
		fmt.Sprintf(`prism_backend_ejected{backend=%q,upstream="web"} 0`, down),
		fmt.Sprintf(`prism_backend_breaker_state{backend=%q,upstream="web"} 0`, backend.URL),
		`prism_upstream_available_backends{upstream="web"} 1`,
		`prism_route_concurrency_in_use{route="/api/"} 0`,
		`prism_route_concurrency_limit{route="/api/"} 8`,
		`prism_route_queue_depth{route="/api/"} 0`,
		`prism_cache_requests_total{result="MISS"} 1`,
		`prism_cache_requests_total{result="HIT"} 1`,
		`prism_cache_entries 1`,
		`prism_cache_max_bytes 1.048576e+06`,
		`prism_cache_waiting 0`,
		// The request metrics share the registry. Nothing went through
		// the access log, which records them.
		`prism_requests_total{code="2xx",route="/",upstream="web"} 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if !strings.Contains(out, "\ngo_goroutines ") {
		t.Error("/metrics lacks the Go runtime's metrics")
	}
	// Only routes with a concurrency limit have its gauges.
	if strings.Contains(out, `prism_route_concurrency_limit{route="/"}`) {
		t.Error(`/metrics has a concurrency limit for route "/", which has none`)
	}

	problems, err := testutil.GatherAndLint(m.Registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "prism_") {
			t.Errorf("lint: %s: %s", p.Metric, p.Text)
		}
	}
}

// TestMetricsFollowReload checks that the gauges describe whichever
// runtime is current, so a backend that a reload removes disappears.
func TestMetricsFollowReload(t *testing.T) {
	m := observability.NewMetrics()
	a := metricsRuntime(t, m, "http://10.0.0.1:80", "http://10.0.0.2:80")
	b := metricsRuntime(t, m, "http://10.0.0.1:80")
	cur := runtime.NewCurrent(a)
	h := New(cur, m, nil, discard)
	removed := `prism_backend_inflight{backend="http://10.0.0.2:80",upstream="web"}`
	if !strings.Contains(scrape(t, h), removed) {
		t.Fatalf("/metrics lacks %s before the reload", removed)
	}
	cur.Swap(b)
	out := scrape(t, h)
	if strings.Contains(out, removed) {
		t.Errorf("/metrics still has %s after the reload", removed)
	}
	if !strings.Contains(out, `prism_upstream_available_backends{upstream="web"} 1`) {
		t.Error("/metrics does not count the new runtime's backends")
	}
}

func TestNoMetrics(t *testing.T) {
	h := New(runtime.NewCurrent(cachingRuntime(t, true)), nil, nil, discard)
	if rec := do(h, "GET", "/metrics", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics without metrics: %d, want 404", rec.Code)
	}
}

func TestBreakerValue(t *testing.T) {
	for s, want := range map[resilience.BreakerState]float64{
		resilience.BreakerClosed:   0,
		resilience.BreakerHalfOpen: 1,
		resilience.BreakerOpen:     2,
	} {
		if got := breakerValue(s); got != want {
			t.Errorf("breakerValue(%v) = %v, want %v", s, got, want)
		}
	}
}
