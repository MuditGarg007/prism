package admin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/runtime"
)

// stateConfig has two upstreams, one with a breaker and outlier detection,
// and three routes with different limits. Nothing is ever sent to the
// backends.
const stateConfig = `
listeners: [{ addr: ":8080" }]
trusted_proxies: [10.0.0.0/8, 127.0.0.1]
cache: { max_size: 2MB }
upstreams:
  web:
    balancer: least_conn
    backends: [{ url: "http://user:hunter2@127.0.0.1:1" }, { url: "http://127.0.0.1:2", weight: 3 }]
    circuit_breaker: { failure_ratio: 0.5, min_requests: 1, open_for: 30s }
    outlier: { consecutive_failures: 5 }
  static:
    backends: [{ url: "http://127.0.0.1:3" }]
    health: { path: /healthz }
routes:
  - { match: { host: API.example.com, path_prefix: /v1/ }, upstream: web, max_concurrency: 4, queue_size: 2,
      rate_limit: { per_ip: 100/s }, retry: { attempts: 2, on: [connect_error, 503] } }
  - { match: { path_prefix: /assets/ }, upstream: static, max_body: 1500, cache: { enabled: true, stale_if_error: 1m } }
  - { match: { path_prefix: / }, upstream: web, headers: { request_set: { X-B: "2", X-A: "1" } } }
`

func stateRuntime(t *testing.T) *runtime.Runtime {
	t.Helper()
	cfg, err := config.Parse([]byte(stateConfig), "")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.Build(cfg, discard, runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	return rt
}

// get serves GET path and decodes the JSON body into v.
func get(t *testing.T, h http.Handler, path string, v any) {
	t.Helper()
	rec := do(h, "GET", path, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET %s: status %d, type %q, body %s", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("GET %s: %v in %s", path, err, rec.Body)
	}
}

func TestRoutes(t *testing.T) {
	rt := stateRuntime(t)
	h := New(runtime.NewCurrent(rt), nil, nil, discard)
	sem := rt.Routes[0].Concurrency
	sem.TryAcquire()
	defer sem.Release()

	var got Routes
	get(t, h, "/routes", &got)
	want := []RouteInfo{
		{
			Name: "api.example.com/v1/", Host: "api.example.com", PathPrefix: "/v1/", Upstream: "web",
			Concurrency: &Concurrency{InUse: 1, Limit: 4},
		},
		{Name: "/assets/", PathPrefix: "/assets/", Upstream: "static", Cache: true},
		{Name: "/", PathPrefix: "/", Upstream: "web"},
	}
	if !reflect.DeepEqual(got.Routes, want) {
		t.Errorf("routes =\n%+v\nwant\n%+v", got.Routes, want)
	}
}

func TestUpstreams(t *testing.T) {
	rt := stateRuntime(t)
	h := New(runtime.NewCurrent(rt), nil, nil, discard)
	web, static := rt.Pools["web"].Backends, rt.Pools["static"].Backends
	web[1].Acquire()
	defer web[1].Release()
	// One failure with min_requests 1 opens the breaker.
	a, ok := web[0].Admit()
	if !ok {
		t.Fatal("closed breaker refused a request")
	}
	web[0].Report(a, resilience.Failure)
	static[0].SetHealthy(false)

	var got Upstreams
	get(t, h, "/upstreams", &got)
	want := []UpstreamInfo{
		{Name: "static", Balancer: "round_robin", Available: 0, Backends: []BackendInfo{
			{URL: "http://127.0.0.1:3", Weight: 1},
		}},
		{Name: "web", Balancer: "least_conn", Available: 1, Backends: []BackendInfo{
			{URL: "http://user:xxxxx@127.0.0.1:1", Weight: 1, Healthy: true, Breaker: "open"},
			{URL: "http://127.0.0.1:2", Weight: 3, Inflight: 1, Healthy: true, Breaker: "closed", Available: true},
		}},
	}
	if !reflect.DeepEqual(got.Upstreams, want) {
		t.Errorf("upstreams =\n%+v\nwant\n%+v", got.Upstreams, want)
	}
}

// TestStateFollowsReload checks that the endpoints read whichever runtime
// is current.
func TestStateFollowsReload(t *testing.T) {
	cur := runtime.NewCurrent(cachingRuntime(t, true))
	h := New(cur, nil, nil, discard)
	cur.Swap(stateRuntime(t))
	var routes Routes
	get(t, h, "/routes", &routes)
	var ups Upstreams
	get(t, h, "/upstreams", &ups)
	if len(routes.Routes) != 3 || len(ups.Upstreams) != 2 {
		t.Errorf("after the swap: %d routes, %d upstreams; want 3 and 2", len(routes.Routes), len(ups.Upstreams))
	}
}
