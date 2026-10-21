package admin

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/runtime"
)

// collector reports the state of the current Runtime when Prometheus
// scrapes: gauges such as inflight requests and breaker states, and the
// cache's counters. Reading the state at scrape time keeps it off the
// request path, and means a reload needs no metric bookkeeping: the next
// scrape simply reads the new Runtime.
type collector struct {
	cur *runtime.Current
}

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, labels, nil)
}

var (
	backendInflight = desc("prism_backend_inflight", "Requests being forwarded to the backend.", "upstream", "backend")
	backendHealthy  = desc("prism_backend_healthy", "1 if health checks pass or are not configured, else 0.", "upstream", "backend")
	backendEjected  = desc("prism_backend_ejected", "1 if outlier detection has ejected the backend, else 0.", "upstream", "backend")
	backendBreaker  = desc("prism_backend_breaker_state",
		"Circuit breaker state: 0 closed, 1 half-open, 2 open. Only backends with a circuit breaker have one.", "upstream", "backend")
	upstreamAvailable = desc("prism_upstream_available_backends",
		"Backends that may be picked: healthy, not ejected and breaker not open.", "upstream")

	routeInUse     = desc("prism_route_concurrency_in_use", "Concurrency slots taken, for routes with max_concurrency.", "route")
	routeLimit     = desc("prism_route_concurrency_limit", "The route's max_concurrency.", "route")
	routeQueue     = desc("prism_route_queue_depth", "Requests waiting for a concurrency slot.", "route")
	shadowInFlight = desc("prism_shadow_inflight", "Shadow copies being sent, for routes with a shadow.", "route")

	cacheRequests  = desc("prism_cache_requests_total", "Requests on caching routes, by X-Cache result.", "result")
	cacheEntries   = desc("prism_cache_entries", "Responses held by the cache.")
	cacheBytes     = desc("prism_cache_bytes", "Bytes held by the cache, keys and headers included.")
	cacheMaxBytes  = desc("prism_cache_max_bytes", "The cache's max_size.")
	cacheEvictions = desc("prism_cache_evictions_total", "Entries evicted to make room.")
	cacheWaiting   = desc("prism_cache_waiting", "Requests waiting on another request's fetch of the same key.")
)

// Describe sends nothing, which makes this an unchecked collector: the
// series it reports change with every reload.
func (c collector) Describe(chan<- *prometheus.Desc) {}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	rt := c.cur.Load()
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}

	for name, p := range rt.Pools {
		available := 0
		for _, b := range p.Backends {
			id := b.String()
			gauge(backendInflight, float64(b.Inflight()), name, id)
			gauge(backendHealthy, bit(b.Healthy()), name, id)
			gauge(backendEjected, bit(b.Ejected()), name, id)
			if br := b.Breaker(); br != nil {
				gauge(backendBreaker, breakerValue(br.State()), name, id)
			}
			if b.Available() {
				available++
			}
		}
		gauge(upstreamAvailable, float64(available), name)
	}

	for _, r := range rt.Routes {
		if s := r.Concurrency; s != nil {
			gauge(routeInUse, float64(s.InUse()), r.Name)
			gauge(routeLimit, float64(s.Cap()), r.Name)
			gauge(routeQueue, float64(s.Waiting()), r.Name)
		}
		if sh := r.Shadow; sh != nil {
			gauge(shadowInFlight, float64(sh.Slots.InUse()), r.Name)
		}
	}

	if cc := rt.Cache; cc != nil {
		n, st := cc.Stats.Snapshot(), cc.Store.Stats()
		counter(cacheRequests, n.Hit, "HIT")
		counter(cacheRequests, n.Miss, "MISS")
		counter(cacheRequests, n.Stale, "STALE")
		counter(cacheRequests, n.Revalidated, "REVALIDATED")
		counter(cacheRequests, n.Bypass, "BYPASS")
		gauge(cacheEntries, float64(st.Entries))
		gauge(cacheBytes, float64(st.Bytes))
		gauge(cacheMaxBytes, float64(cc.MaxSize))
		counter(cacheEvictions, st.Evictions)
		gauge(cacheWaiting, float64(cc.Flight.Waiting()))
	}
}

func bit(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// breakerValue orders breaker states by how much traffic they refuse, so
// an alert can test for a value above 0.
func breakerValue(s resilience.BreakerState) float64 {
	switch s {
	case resilience.BreakerHalfOpen:
		return 1
	case resilience.BreakerOpen:
		return 2
	}
	return 0
}
