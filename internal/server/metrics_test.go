package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/runtime"
	"github.com/prometheus/client_golang/prometheus"
)

// TestAccessLogMetrics checks that a proxied request and an unmatched one
// are counted under their routes, and that the backend's response time is
// recorded.
func TestAccessLogMetrics(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer backend.Close()
	cfg, err := config.Parse(fmt.Appendf(nil, `
listeners: [{ addr: ":8080" }]
upstreams:
  api: { backends: [{ url: %q }] }
routes:
  - { match: { host: api.test, path_prefix: /v1/ }, upstream: api }
`, backend.URL), "")
	if err != nil {
		t.Fatal(err)
	}
	logs, log := newTestLogger()
	m := observability.NewMetrics()
	rt, err := runtime.Build(cfg, log, runtime.Options{Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	_, urls := start(t, []config.Listener{plainListener()}, runtime.NewCurrent(rt), log)

	for _, host := range []string{"api.test", "other.test"} {
		req, _ := http.NewRequest("GET", urls[0]+"/v1/x", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// The metrics are recorded just before the access log line.
	logs.waitFor(t, "status=200")
	logs.waitFor(t, "status=404")

	for _, tt := range []struct {
		name   string
		labels prometheus.Labels
		want   float64
	}{
		{"prism_requests_total", prometheus.Labels{"route": "api.test/v1/", "upstream": "api", "code": "2xx"}, 1},
		{"prism_requests_total", prometheus.Labels{"route": "", "upstream": "", "code": "4xx"}, 1},
		{"prism_request_duration_seconds", prometheus.Labels{"route": "api.test/v1/", "upstream": "api"}, 1},
		{"prism_upstream_response_seconds", prometheus.Labels{"upstream": "api", "backend": backend.URL}, 1},
	} {
		if got := sample(t, m.Registry, tt.name, tt.labels); got != tt.want {
			t.Errorf("%s%v = %v, want %v", tt.name, tt.labels, got, tt.want)
		}
	}
}

// sample returns the value of the series of name with exactly labels: a
// counter's value, or a histogram's count. It returns -1 if there is none.
func sample(t *testing.T, g prometheus.Gatherer, name string, labels prometheus.Labels) float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, mt := range mf.GetMetric() {
			if len(mt.GetLabel()) != len(labels) {
				continue
			}
			for _, l := range mt.GetLabel() {
				if v, ok := labels[l.GetName()]; !ok || v != l.GetValue() {
					continue next
				}
			}
			switch {
			case mt.Counter != nil:
				return mt.Counter.GetValue()
			case mt.Gauge != nil:
				return mt.Gauge.GetValue()
			case mt.Histogram != nil:
				return float64(mt.Histogram.GetSampleCount())
			}
		}
	}
	return -1
}
