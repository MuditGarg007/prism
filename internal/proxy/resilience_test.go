package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/upstream"
)

func TestProxyBreakerOpens(t *testing.T) {
	backend, hits := statusBackend(t, "down", 500)
	u := testUpstream(backend.URL)
	u.CircuitBreaker = &config.CircuitBreaker{FailureRatio: 1, MinRequests: 3, OpenFor: time.Hour, Window: 10 * time.Second, HalfOpenProbes: 1}
	rt := &Route{}
	h := newHandler(t, u, rt, slog.New(slog.DiscardHandler))
	for i := range 3 {
		if w := send(h, "GET", nil); w.Code != 500 {
			t.Fatalf("request %d: got %d, want the backend's 500", i, w.Code)
		}
	}
	if s := rt.Pool.Backends[0].Breaker().State(); s != resilience.BreakerOpen {
		t.Fatalf("breaker %v after 3 failures, want open", s)
	}
	w := send(h, "GET", nil)
	if w.Code != http.StatusServiceUnavailable || w.an.Error != "no_backend" || hits.Load() != 3 {
		t.Errorf("got %d %q with %d backend hits; want 503 no_backend and the backend left alone", w.Code, w.an.Error, hits.Load())
	}
}

func TestProxyOutlierEjects(t *testing.T) {
	failing, failHits := statusBackend(t, "failing", 503)
	ok, okHits := statusBackend(t, "ok", 200)
	u := poolUpstream(failing.URL, ok.URL)
	u.Outlier = &config.Outlier{ConsecutiveFailures: 2, BaseEjection: time.Hour, MaxEjection: time.Hour, MaxEjectionPercent: 50}
	h := newHandler(t, u, &Route{}, slog.New(slog.DiscardHandler))
	// Round robin alternates, so requests 0 and 2 fail and eject it.
	for range 4 {
		send(h, "GET", nil)
	}
	for i := range 10 {
		if w := send(h, "GET", nil); w.Code != 200 {
			t.Fatalf("request %d after the ejection: got %d, want 200", i, w.Code)
		}
	}
	if failHits.Load() != 2 || okHits.Load() != 12 {
		t.Errorf("hits: failing %d, ok %d; want 2 and 12", failHits.Load(), okHits.Load())
	}
}

// brokenBody returns some bytes and then an error, as a client connection
// that drops mid-upload does.
type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("client connection reset")
	}
	b.sent = true
	return copy(p, "partial"), nil
}

// TestProxyClientFaultsAreNeutral checks that failures the client causes
// never count against a backend. The breaker would open on a single failure.
func TestProxyClientFaultsAreNeutral(t *testing.T) {
	backend, _ := statusBackend(t, "ok", 200)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	for _, tt := range []struct {
		name string
		url  string
		req  func() *http.Request
		kind string
	}{
		{"broken request body", backend.URL, func() *http.Request {
			return httptest.NewRequest("POST", "/", &brokenBody{})
		}, "client_body"},
		{"body over max_body", backend.URL, func() *http.Request {
			return httptest.NewRequest("POST", "/", io.LimitReader(strings.NewReader(strings.Repeat("x", 100)), 100))
		}, "body_too_large"},
		{"client cancels", slow.URL, func() *http.Request {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			t.Cleanup(cancel)
			return httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		}, "timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := testUpstream(tt.url)
			u.CircuitBreaker = &config.CircuitBreaker{FailureRatio: 1, MinRequests: 1, OpenFor: time.Hour, Window: 10 * time.Second, HalfOpenProbes: 1}
			u.Outlier = &config.Outlier{ConsecutiveFailures: 1, BaseEjection: time.Hour, MaxEjection: time.Hour, MaxEjectionPercent: 100}
			rt := &Route{MaxBody: 10}
			h := newHandler(t, u, rt, slog.New(slog.DiscardHandler))
			w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, tt.req())
			if w.an.Error != tt.kind {
				t.Fatalf("got %d %q, want kind %q", w.Code, w.an.Error, tt.kind)
			}
			b := rt.Pool.Backends[0]
			if s := b.Breaker().State(); s != resilience.BreakerClosed || !b.Available() {
				t.Errorf("after a %s failure: breaker %v, available %v; want closed and available", w.an.Error, s, b.Available())
			}
		})
	}
}

// TestProxyBodyFailureCounts checks that a backend that sends its headers
// and then drops the body counts as failing, though the headers alone looked
// like a success.
func TestProxyBodyFailureCounts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "only ten..")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer backend.Close()
	u := testUpstream(backend.URL)
	u.CircuitBreaker = &config.CircuitBreaker{FailureRatio: 1, MinRequests: 1, OpenFor: time.Hour, Window: 10 * time.Second, HalfOpenProbes: 1}
	u.Outlier = &config.Outlier{ConsecutiveFailures: 1, BaseEjection: time.Hour, MaxEjection: time.Hour, MaxEjectionPercent: 100}
	rt := &Route{}
	front := serve(t, newHandler(t, u, rt, slog.New(slog.DiscardHandler)))

	// The proxy has not flushed the headers yet, so the client sees the
	// connection close either before or after them.
	if resp, err := http.Get(front + "/"); err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Error("the client read the cut-off body without an error")
		}
	}
	b := rt.Pool.Backends[0]
	if s := b.Breaker().State(); s != resilience.BreakerOpen || b.Available() {
		t.Errorf("after a broken response body: breaker %v, available %v; want open and unavailable", s, b.Available())
	}
}

// lateBalancer picks the first backend not tried yet, without checking
// whether it is available. It stands in for the race where a breaker's Ready
// says yes and its Allow, a moment later, says no.
type lateBalancer struct{ bs []*upstream.Backend }

func (l lateBalancer) Pick(c upstream.Choice) (*upstream.Backend, error) {
	for _, b := range l.bs {
		if !slices.Contains(c.Tried, b) {
			return b, nil
		}
	}
	return nil, upstream.ErrNoBackend
}

func TestPickSkipsRefusingBackend(t *testing.T) {
	u := poolUpstream("http://127.0.0.1:1", "http://127.0.0.1:2")
	u.CircuitBreaker = &config.CircuitBreaker{FailureRatio: 1, MinRequests: 1, OpenFor: time.Hour, Window: 10 * time.Second, HalfOpenProbes: 1}
	pool, err := upstream.NewPool("test", u, func(bs []*upstream.Backend) upstream.Balancer { return lateBalancer{bs} }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	open := func(b *upstream.Backend) {
		adm, _ := b.Admit()
		b.Report(adm, resilience.Failure)
	}
	open(pool.Backends[0])

	var c upstream.Choice
	b, _, err := pick(pool, &c)
	if err != nil || b != pool.Backends[1] {
		t.Fatalf("pick = %v, %v; want the second backend", b, err)
	}
	if len(c.Tried) != 1 || c.Tried[0] != pool.Backends[0] {
		t.Errorf("Tried = %v, want the refusing backend, so a retry skips it too", c.Tried)
	}

	open(pool.Backends[1])
	if _, _, err := pick(pool, &upstream.Choice{}); !errors.Is(err, upstream.ErrNoBackend) {
		t.Errorf("pick with every breaker open: %v, want ErrNoBackend", err)
	}
}
