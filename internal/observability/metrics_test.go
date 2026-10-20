package observability

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRouteMetricsObserve(t *testing.T) {
	m := NewMetrics()
	rm := m.Route("api.test/v1/", "api")
	for _, r := range []Request{
		{Status: 200, Duration: 3 * time.Millisecond, Attempts: 1},
		{Status: 204, Duration: time.Millisecond, Attempts: 3},
		{Status: 499, Attempts: 1, ErrorKind: "client_closed"},
		{Status: 502, Attempts: 2, BudgetExhausted: true, ErrorKind: "upstream"},
		{Status: 503, ErrorKind: "overloaded"},
		{Status: 700, Attempts: 1}, // a backend may send any three digits
	} {
		rm.Observe(r)
	}
	// Another handle for the same route, as a reload makes, adds to the
	// same series.
	m.Route("api.test/v1/", "api").Observe(Request{Status: 200, Attempts: 1})
	m.Unmatched().Observe(Request{Status: 404})

	counters := map[string]float64{
		`prism_requests_total{code="2xx",route="api.test/v1/",upstream="api"}`:   3,
		`prism_requests_total{code="4xx",route="api.test/v1/",upstream="api"}`:   1,
		`prism_requests_total{code="5xx",route="api.test/v1/",upstream="api"}`:   2,
		`prism_requests_total{code="other",route="api.test/v1/",upstream="api"}`: 1,
		`prism_requests_total{code="1xx",route="api.test/v1/",upstream="api"}`:   0,
		`prism_requests_total{code="4xx",route="",upstream=""}`:                  1,
		`prism_retries_total{route="api.test/v1/"}`:                              3,
		`prism_retry_budget_exhausted_total{route="api.test/v1/"}`:               1,
		`prism_proxy_errors_total{kind="upstream",route="api.test/v1/"}`:         1,
		`prism_proxy_errors_total{kind="overloaded",route="api.test/v1/"}`:       1,
		`prism_proxy_errors_total{kind="client_closed",route="api.test/v1/"}`:    1,
	}
	got := gather(t, m)
	for series, want := range counters {
		if v, ok := got[series]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", series, v, ok, want)
		}
	}
	if c := got[`prism_request_duration_seconds_count{route="api.test/v1/",upstream="api"}`]; c != 7 {
		t.Errorf("duration count %v, want 7", c)
	}
	if b := got[`prism_request_duration_seconds_bucket{le="0.0025",route="api.test/v1/",upstream="api"}`]; b != 6 {
		t.Errorf("observations up to 2.5ms: %v, want 6", b)
	}
}

func TestBackendLatency(t *testing.T) {
	m := NewMetrics()
	m.Backend("api", "http://10.0.0.1:8080").Observe(0.02)
	got := gather(t, m)
	if c := got[`prism_upstream_response_seconds_count{backend="http://10.0.0.1:8080",upstream="api"}`]; c != 1 {
		t.Errorf("upstream response count %v, want 1", c)
	}
}

func TestReloadMetrics(t *testing.T) {
	m := NewMetrics()
	start := time.Unix(1000, 0)
	m.ConfigLoaded(start)
	m.Reloaded(errors.New("bad config"), start.Add(time.Second))
	m.Reloaded(nil, start.Add(2*time.Second))
	m.Reloaded(errors.New("bad config"), start.Add(3*time.Second))
	got := gather(t, m)
	for series, want := range map[string]float64{
		`prism_config_reloads_total{result="success"}`:         1,
		`prism_config_reloads_total{result="failure"}`:         2,
		`prism_config_last_reload_successful{}`:                0,
		`prism_config_last_reload_success_timestamp_seconds{}`: 1002,
	} {
		if v, ok := got[series]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", series, v, ok, want)
		}
	}
}

// TestPrune checks that a reload deletes the series of a removed route, of
// a route's previous upstream and of a removed backend, and nothing else.
func TestPrune(t *testing.T) {
	m := NewMetrics()
	fail := Request{Status: 502, Attempts: 2, BudgetExhausted: true, ErrorKind: "upstream"}
	m.Route("a/", "a").Observe(fail)
	m.Route("b/", "b").Observe(fail)
	m.Route("c/", "a").Observe(fail)
	m.Backend("a", "http://a1").Observe(0.01)
	m.Backend("a", "http://a2").Observe(0.01)
	old := Series{
		Routes:   map[RouteSeries]bool{{"a/", "a"}: true, {"b/", "b"}: true, {"c/", "a"}: true},
		Backends: map[BackendSeries]bool{{"a", "http://a1"}: true, {"a", "http://a2"}: true},
	}
	next := Series{ // b/ removed, c/ moved to upstream b, a2 removed
		Routes:   map[RouteSeries]bool{{"a/", "a"}: true, {"c/", "b"}: true},
		Backends: map[BackendSeries]bool{{"a", "http://a1"}: true},
	}
	m.Prune(old, next)
	m.Route("c/", "b") // as the new runtime's Build does

	got := gather(t, m)
	present := func(series string) bool { _, ok := got[series]; return ok }
	for _, series := range []string{
		`prism_requests_total{code="5xx",route="a/",upstream="a"}`,
		`prism_request_duration_seconds_count{route="a/",upstream="a"}`,
		`prism_retries_total{route="a/"}`,
		`prism_proxy_errors_total{kind="upstream",route="a/"}`,
		// c/ keeps its route-only series, and counts on under its new upstream.
		`prism_retries_total{route="c/"}`,
		`prism_proxy_errors_total{kind="upstream",route="c/"}`,
		`prism_requests_total{code="5xx",route="c/",upstream="b"}`,
		`prism_upstream_response_seconds_count{backend="http://a1",upstream="a"}`,
		`prism_requests_total{code="2xx",route="",upstream=""}`,
	} {
		if !present(series) {
			t.Errorf("%s was deleted", series)
		}
	}
	for _, series := range []string{
		`prism_requests_total{code="5xx",route="b/",upstream="b"}`,
		`prism_requests_total{code="2xx",route="b/",upstream="b"}`,
		`prism_request_duration_seconds_count{route="b/",upstream="b"}`,
		`prism_retries_total{route="b/"}`,
		`prism_retry_budget_exhausted_total{route="b/"}`,
		`prism_proxy_errors_total{kind="upstream",route="b/"}`,
		`prism_requests_total{code="5xx",route="c/",upstream="a"}`,
		`prism_request_duration_seconds_count{route="c/",upstream="a"}`,
		`prism_upstream_response_seconds_count{backend="http://a2",upstream="a"}`,
	} {
		if present(series) {
			t.Errorf("%s is still there", series)
		}
	}
}

func TestNilRouteMetrics(t *testing.T) {
	var rm *RouteMetrics
	rm.Observe(Request{Status: 200}) // must not panic
}

func TestMetricsLint(t *testing.T) {
	m := NewMetrics()
	m.Route("a/", "a").Observe(Request{Status: 200, Attempts: 2, ErrorKind: "upstream"})
	problems, err := testutil.GatherAndLint(m.Registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "prism_") {
			t.Errorf("%s: %s", p.Metric, p.Text)
		}
	}
}

// TestObserveAllocs checks that recording a request that did not fail
// allocates nothing, since it runs for every request.
func TestObserveAllocs(t *testing.T) {
	rm := NewMetrics().Route("a/", "a")
	r := Request{Status: 200, Duration: time.Millisecond, Attempts: 2}
	if n := testing.AllocsPerRun(100, func() { rm.Observe(r) }); n != 0 {
		t.Errorf("Observe allocates %v times, want 0", n)
	}
}

// gather returns every prism_ sample of m, keyed by its name and sorted
// labels as the text format writes them.
func gather(t *testing.T, m *Metrics) map[string]float64 {
	t.Helper()
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]float64)
	for _, mf := range mfs {
		name := mf.GetName()
		if !strings.HasPrefix(name, "prism_") {
			continue
		}
		for _, mt := range mf.GetMetric() {
			labels := make([]string, 0, len(mt.GetLabel()))
			for _, l := range mt.GetLabel() {
				labels = append(labels, l.GetName()+`="`+l.GetValue()+`"`)
			}
			key := func(suffix string, extra ...string) string {
				all := append(append([]string(nil), extra...), labels...)
				slices.Sort(all)
				return name + suffix + "{" + strings.Join(all, ",") + "}"
			}
			switch {
			case mt.Counter != nil:
				out[key("")] = mt.Counter.GetValue()
			case mt.Gauge != nil:
				out[key("")] = mt.Gauge.GetValue()
			case mt.Histogram != nil:
				h := mt.Histogram
				out[key("_count")] = float64(h.GetSampleCount())
				for _, b := range h.GetBucket() {
					out[key("_bucket", `le="`+strconv.FormatFloat(b.GetUpperBound(), 'g', -1, 64)+`"`)] = float64(b.GetCumulativeCount())
				}
			}
		}
	}
	return out
}

func BenchmarkObserve(b *testing.B) {
	rm := NewMetrics().Route("a/", "a")
	r := Request{Status: 200, Duration: time.Millisecond, Attempts: 1}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rm.Observe(r)
		}
	})
}
