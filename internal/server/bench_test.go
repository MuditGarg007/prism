package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/runtime"
)

// stubBackend answers every request from memory, so a benchmark through it
// counts Prism's own work and none of a backend's or a socket's.
type stubBackend struct {
	header http.Header
	body   []byte
}

func newStubBackend(cacheControl string) *stubBackend {
	body := []byte(strings.Repeat("x", 1024))
	h := http.Header{
		"Content-Type":   {"text/plain"},
		"Content-Length": {strconv.Itoa(len(body))},
		"Date":           {time.Now().UTC().Format(http.TimeFormat)},
		"Server":         {"stub"},
	}
	if cacheControl != "" {
		h.Set("Cache-Control", cacheControl)
	}
	return &stubBackend{header: h, body: body}
}

func (s *stubBackend) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		r.Body.Close()
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        s.header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(s.body)),
		ContentLength: int64(len(s.body)),
		Request:       r,
	}, nil
}

// discardWriter keeps only the status, so the benchmark measures the
// handler rather than a recorder.
type discardWriter struct {
	h    http.Header
	code int
}

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(code int)        { w.code = code }

// benchConfigs are the routes BenchmarkPipeline runs. bare is a route with
// nothing on it; full has what a production route usually has; cache_hit
// is served from the cache after the first request.
var benchConfigs = []struct {
	name, cacheControl, yaml string
}{
	{"bare", "", `
listeners: [{ addr: ":8080" }]
upstreams:
  api: { backends: [{ url: "http://backend.bench" }] }
routes:
  - { match: { path_prefix: / }, upstream: api }
`},
	{"full", "", `
listeners: [{ addr: ":8080" }]
upstreams:
  api:
    balancer: least_conn
    backends: [{ url: "http://backend.bench" }, { url: "http://backend2.bench" }]
    circuit_breaker: { failure_ratio: 0.5, min_requests: 20, open_for: 30s }
routes:
  - match: { path_prefix: / }
    upstream: api
    headers: { request_set: { X-Env: bench }, response_remove: [Server] }
    rate_limit: { per_ip: "1000000/s", burst: 1000000 }
    max_body: 1MB
    max_concurrency: 1000
    retry: { attempts: 1, on: [connect_error, 502, 503], budget_ratio: 0.2 }
`},
	{"cache_hit", "max-age=3600", `
listeners: [{ addr: ":8080" }]
cache: { max_size: 16MB }
upstreams:
  api: { backends: [{ url: "http://backend.bench" }] }
routes:
  - { match: { path_prefix: / }, upstream: api, cache: { enabled: true } }
`},
}

// BenchmarkPipeline sends a GET through everything Prism does for a
// request between net/http's server and its client: the access log (JSON,
// discarded), metrics, the proxy and the transport call. The backend is
// stubBackend, whose own allocations BenchmarkPipeline/stub reports, so
// they can be subtracted.
func BenchmarkPipeline(b *testing.B) {
	b.Run("stub", func(b *testing.B) {
		s := newStubBackend("")
		r := httptest.NewRequest("GET", "http://backend.bench/", nil)
		b.ReportAllocs()
		for b.Loop() {
			resp, _ := s.RoundTrip(r)
			io.Copy(io.Discard, resp.Body)
		}
	})
	for _, bc := range benchConfigs {
		b.Run(bc.name, func(b *testing.B) {
			cfg, err := config.Parse([]byte(bc.yaml), "")
			if err != nil {
				b.Fatal(err)
			}
			log := slog.New(slog.NewJSONHandler(io.Discard, nil))
			rt, err := runtime.Build(cfg, log, runtime.Options{Metrics: observability.NewMetrics()})
			if err != nil {
				b.Fatal(err)
			}
			defer rt.Close()
			stub := newStubBackend(bc.cacheControl)
			for _, p := range rt.Pools {
				for _, be := range p.Backends {
					be.Transport.RegisterProtocol("http", stub)
				}
			}
			h := accessLog(runtime.NewCurrent(rt), log, nil)
			r := httptest.NewRequest("GET", "http://api.bench/v1/items?page=2", nil)
			r.Header.Set("User-Agent", "bench")
			r.Header.Set("Accept", "*/*")
			w := &discardWriter{h: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				clear(w.h)
				w.code = 0
				h.ServeHTTP(w, r)
				if w.code != http.StatusOK {
					b.Fatalf("status %d", w.code)
				}
			}
		})
	}
}
