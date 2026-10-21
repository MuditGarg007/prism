package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// durationBuckets cover a fast cache hit up to a slow backend, in seconds.
var durationBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

// codeClasses are the code label values, indexed by status/100. Index 0
// holds statuses outside 100–599, which a backend can send.
var codeClasses = [6]string{"other", "1xx", "2xx", "3xx", "4xx", "5xx"}

// Metrics holds the Prometheus metrics that requests update. It outlives
// any one config: a reload resolves new handles from the same vectors, so
// series of routes and backends that still exist carry on counting.
type Metrics struct {
	// Registry holds every Prism metric, plus the Go runtime and process
	// collectors. The admin API serves it.
	Registry *prometheus.Registry

	requests        *prometheus.CounterVec   // route, upstream, code
	duration        *prometheus.HistogramVec // route, upstream
	upstream        *prometheus.HistogramVec // upstream, backend
	retries         *prometheus.CounterVec   // route
	budgetExhausted *prometheus.CounterVec   // route
	errors          *prometheus.CounterVec   // route, kind
	shadow          *prometheus.CounterVec   // route, result

	reloads      *prometheus.CounterVec // result
	lastLoadOK   prometheus.Gauge
	lastLoadTime prometheus.Gauge
	unmatched    *RouteMetrics
}

// NewMetrics registers Prism's metrics in a new registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_requests_total",
			Help: "Requests served, by route, upstream and status class. Unmatched requests have an empty route.",
		}, []string{"route", "upstream", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "prism_request_duration_seconds",
			Help:    "Time from receiving a request to finishing its response, by route and upstream.",
			Buckets: durationBuckets,
		}, []string{"route", "upstream"}),
		upstream: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "prism_upstream_response_seconds",
			Help:    "Time from sending a request to a backend to receiving its response headers, per attempt that got a response.",
			Buckets: durationBuckets,
		}, []string{"upstream", "backend"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_retries_total",
			Help: "Attempts beyond the first, by route.",
		}, []string{"route"}),
		budgetExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_retry_budget_exhausted_total",
			Help: "Requests whose retry the route's retry budget refused.",
		}, []string{"route"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_proxy_errors_total",
			Help: "Requests that failed or were refused, by route and the error kind the access log shows.",
		}, []string{"route", "kind"}),
		shadow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_shadow_requests_total",
			Help: "Shadow copies of requests, by route and result: sent (a response came back), error, busy (dropped at max_concurrency) or body_skipped (the body was too large, of unknown length, or not complete in time).",
		}, []string{"route", "result"}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_config_reloads_total",
			Help: "Config reloads, from SIGHUP or the admin API, by result: success or failure.",
		}, []string{"result"}),
		lastLoadOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "prism_config_last_reload_successful",
			Help: "1 when the last config load, at startup or by a reload, succeeded, 0 when it failed and the previous config is still serving.",
		}),
		lastLoadTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "prism_config_last_reload_success_timestamp_seconds",
			Help: "Unix time of the last successful config load, at startup or by a reload.",
		}),
	}
	m.Registry.MustRegister(
		m.requests, m.duration, m.upstream, m.retries, m.budgetExhausted, m.errors, m.shadow,
		m.reloads, m.lastLoadOK, m.lastLoadTime,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	for _, result := range []string{"success", "failure"} {
		m.reloads.WithLabelValues(result)
	}
	m.unmatched = m.Route("", "")
	return m
}

// ConfigLoaded records that the config at startup loaded at now.
func (m *Metrics) ConfigLoaded(now time.Time) {
	m.lastLoadOK.Set(1)
	m.lastLoadTime.Set(float64(now.UnixNano()) / 1e9)
}

// Reloaded records a reload that finished at now, which failed when err is
// set.
func (m *Metrics) Reloaded(err error, now time.Time) {
	if err != nil {
		m.reloads.WithLabelValues("failure").Inc()
		m.lastLoadOK.Set(0)
		return
	}
	m.reloads.WithLabelValues("success").Inc()
	m.ConfigLoaded(now)
}

// RouteSeries labels the series of a route that forwards to an upstream.
type RouteSeries struct{ Route, Upstream string }

// BackendSeries labels the series of a backend in an upstream.
type BackendSeries struct{ Upstream, Backend string }

// Series are the routes and backends one config records into.
type Series struct {
	Routes   map[RouteSeries]bool
	Backends map[BackendSeries]bool
	Shadows  map[string]bool // routes that send shadow copies
}

// Prune deletes the series of the routes and backends in old that next no
// longer has, so that a reload doesn't leave them frozen at their last
// values. Requests still finishing on the old config afterwards are not
// counted. A route that comes back later starts again from zero, which
// rate() reads as a counter reset.
func (m *Metrics) Prune(old, next Series) {
	names := map[string]bool{}
	for rs := range next.Routes {
		names[rs.Route] = true
	}
	for rs := range old.Routes {
		if !next.Routes[rs] {
			l := prometheus.Labels{"route": rs.Route, "upstream": rs.Upstream}
			m.requests.DeletePartialMatch(l)
			m.duration.Delete(l)
		}
		if !names[rs.Route] {
			l := prometheus.Labels{"route": rs.Route}
			m.retries.Delete(l)
			m.budgetExhausted.Delete(l)
			m.errors.DeletePartialMatch(l)
		}
	}
	for route := range old.Shadows {
		if !next.Shadows[route] {
			m.shadow.DeletePartialMatch(prometheus.Labels{"route": route})
		}
	}
	for bs := range old.Backends {
		if !next.Backends[bs] {
			m.upstream.Delete(prometheus.Labels{"upstream": bs.Upstream, "backend": bs.Backend})
		}
	}
}

// RouteMetrics are one route's handles, resolved once when the route is
// built so that a request never looks a series up by its labels. A nil
// *RouteMetrics records nothing.
type RouteMetrics struct {
	requests        [len(codeClasses)]prometheus.Counter
	duration        prometheus.Observer
	retries         prometheus.Counter
	budgetExhausted prometheus.Counter
	errors          *prometheus.CounterVec // curried with the route
}

// Route returns the handles of the route named name, which forwards to
// upstream. It may be called again for the same route, after a reload, and
// returns handles to the same series.
func (m *Metrics) Route(name, upstream string) *RouteMetrics {
	rm := &RouteMetrics{
		duration:        m.duration.WithLabelValues(name, upstream),
		retries:         m.retries.WithLabelValues(name),
		budgetExhausted: m.budgetExhausted.WithLabelValues(name),
		errors:          m.errors.MustCurryWith(prometheus.Labels{"route": name}),
	}
	for i, class := range codeClasses {
		rm.requests[i] = m.requests.WithLabelValues(name, upstream, class)
	}
	return rm
}

// Unmatched returns the handles for requests that match no route.
func (m *Metrics) Unmatched() *RouteMetrics { return m.unmatched }

// Backend returns the observer of how long backend, in upstream, takes to
// send response headers.
func (m *Metrics) Backend(upstream, backend string) prometheus.Observer {
	return m.upstream.WithLabelValues(upstream, backend)
}

// Request is how one request ended, as the access log saw it.
type Request struct {
	Status          int
	Duration        time.Duration
	Attempts        int    // backends tried, counting the first
	BudgetExhausted bool   // a retry was refused by the budget
	ErrorKind       string // "" when nothing failed
}

// Observe records one finished request.
func (rm *RouteMetrics) Observe(r Request) {
	if rm == nil {
		return
	}
	class := 0
	if r.Status >= 100 && r.Status < 600 {
		class = r.Status / 100
	}
	rm.requests[class].Inc()
	rm.duration.Observe(r.Duration.Seconds())
	if r.Attempts > 1 {
		rm.retries.Add(float64(r.Attempts - 1))
	}
	if r.BudgetExhausted {
		rm.budgetExhausted.Inc()
	}
	if r.ErrorKind != "" {
		// Kinds are a short fixed list, so the lookup only runs on
		// failures and the series stay few.
		rm.errors.WithLabelValues(r.ErrorKind).Inc()
	}
}

// ShadowResult is how a shadow copy of a request ended.
type ShadowResult int

const (
	ShadowSent    ShadowResult = iota // a response came back, whatever its status
	ShadowError                       // no response: no backend, a connect error or a timeout
	ShadowBusy                        // dropped, because max_concurrency copies were running
	ShadowSkipped                     // not sent, because of the request's body
)

var shadowResults = [...]string{"sent", "error", "busy", "body_skipped"}

// ShadowMetrics are one route's shadow counters. A nil *ShadowMetrics
// records nothing.
type ShadowMetrics struct {
	results [len(shadowResults)]prometheus.Counter
}

// Shadow returns the shadow counters of the route named route.
func (m *Metrics) Shadow(route string) *ShadowMetrics {
	sm := &ShadowMetrics{}
	for i, result := range shadowResults {
		sm.results[i] = m.shadow.WithLabelValues(route, result)
	}
	return sm
}

// Inc counts one shadow copy that ended with r.
func (sm *ShadowMetrics) Inc(r ShadowResult) {
	if sm != nil {
		sm.results[r].Inc()
	}
}
