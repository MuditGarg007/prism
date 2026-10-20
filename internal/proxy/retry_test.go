package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/resilience"
)

// statusBackend reads the whole request body, then answers with status and
// "name:<bytes read>", counting the requests that reach it.
func statusBackend(t *testing.T, name string, status int) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		fmt.Fprintf(w, "%s:%d", name, n)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// poolUpstream is testUpstream with one backend per URL, in order. The
// round robin starts at the first, so a new handler's first request goes to
// urls[0] and its first retry to urls[1].
func poolUpstream(urls ...string) *config.Upstream {
	u := testUpstream(urls[0])
	u.Backends = nil
	for _, url := range urls {
		u.Backends = append(u.Backends, config.Backend{URL: url, Weight: 1})
	}
	return u
}

func retryPolicy(t *testing.T, attempts int, on ...string) *resilience.Retry {
	t.Helper()
	p, err := resilience.NewRetry(attempts, on)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// send serves one request with h directly and returns what the client and
// the access log would see.
func send(h http.Handler, method string, body io.Reader) *annotatedRecorder {
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest(method, "/", body))
	return w
}

// bodies are request bodies of each kind the proxy treats differently: none,
// read up front for replay, and streamed through once.
var bodies = []struct {
	name string
	size int
	body func() io.Reader
}{
	{"no body", 0, func() io.Reader { return nil }},
	{"small length", 1000, func() io.Reader { return bytes.NewReader(make([]byte, 1000)) }},
	{"length at the replay limit", replayLimit, func() io.Reader { return bytes.NewReader(make([]byte, replayLimit)) }},
	{"length over the replay limit", 100_000, func() io.Reader { return bytes.NewReader(make([]byte, 100_000)) }},
	// MultiReader hides the length, so the body is sent chunked.
	{"chunked", 1000, func() io.Reader { return io.MultiReader(bytes.NewReader(make([]byte, 1000))) }},
}

// TestProxyRetryConnectError checks that a failed connection is retried on
// another backend for any method and any body, because the backend that
// failed never saw a byte of it.
func TestProxyRetryConnectError(t *testing.T) {
	live, _ := statusBackend(t, "live", 200)
	for _, method := range []string{"GET", "POST"} {
		for _, b := range bodies {
			t.Run(method+" "+b.name, func(t *testing.T) {
				logs, log := newTestLogger()
				rt := &Route{Retry: retryPolicy(t, 1, "connect_error")}
				h := newHandler(t, poolUpstream("http://"+closedAddr(t), live.URL), rt, log)
				w := send(h, method, b.body())
				if want := fmt.Sprintf("live:%d", b.size); w.Code != 200 || w.Body.String() != want {
					t.Fatalf("got %d %q, want 200 %q", w.Code, w.Body, want)
				}
				if w.an.Attempts != 2 || w.an.Backend != rt.Pool.Backends[1] || w.an.Error != "" {
					t.Errorf("annotations = %+v, want 2 attempts ending on the live backend", w.an)
				}
				if out := logs.String(); !strings.Contains(out, "retrying") || !strings.Contains(out, "kind=connect") {
					t.Errorf("the failed attempt was not logged: %s", out)
				}
			})
		}
	}
}

// TestProxyRetryStatus checks which requests a retryable 5xx is retried
// for: only idempotent ones whose body can be sent again in full.
func TestProxyRetryStatus(t *testing.T) {
	tests := []struct {
		method string
		body   int // index into bodies
		retry  bool
	}{
		{"GET", 0, true},
		{"PUT", 1, true},
		{"PUT", 2, true},
		{"DELETE", 0, true},
		// Not idempotent, so the backend may already have acted on it.
		{"POST", 0, false},
		{"POST", 1, false},
		{"PATCH", 1, false},
		// The first backend read the streamed body, so it cannot be sent again.
		{"PUT", 3, false},
		{"PUT", 4, false},
	}
	for _, tt := range tests {
		b := bodies[tt.body]
		t.Run(tt.method+" "+b.name, func(t *testing.T) {
			failing, failHits := statusBackend(t, "failing", 503)
			ok, okHits := statusBackend(t, "ok", 200)
			h := newHandler(t, poolUpstream(failing.URL, ok.URL), &Route{Retry: retryPolicy(t, 1, "503")}, slog.New(slog.DiscardHandler))
			w := send(h, tt.method, b.body())
			want, attempts := fmt.Sprintf("failing:%d", b.size), 1
			if tt.retry {
				want, attempts = fmt.Sprintf("ok:%d", b.size), 2
			}
			if w.Body.String() != want || w.an.Attempts != attempts {
				t.Errorf("got %d %q after %d attempts, want %q after %d", w.Code, w.Body, w.an.Attempts, want, attempts)
			}
			if failHits.Load() != 1 || okHits.Load() != int64(attempts-1) {
				t.Errorf("backends got %d and %d requests", failHits.Load(), okHits.Load())
			}
		})
	}
}

func TestProxyRetryUnlistedStatus(t *testing.T) {
	failing, _ := statusBackend(t, "failing", 500)
	ok, okHits := statusBackend(t, "ok", 200)
	h := newHandler(t, poolUpstream(failing.URL, ok.URL), &Route{Retry: retryPolicy(t, 1, "502", "503")}, slog.New(slog.DiscardHandler))
	if w := send(h, "GET", nil); w.Code != 500 || okHits.Load() != 0 {
		t.Errorf("a 500 not in retry.on got %d after %d retries", w.Code, okHits.Load())
	}
}

// TestProxyRetryLimits checks that retries stop at retry.attempts and when
// every backend has been tried, and that the last attempt's response goes to
// the client unchanged.
func TestProxyRetryLimits(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		want     int // attempts made, and so backends hit
	}{
		{"attempts cap", 1, 2},
		{"pool runs out", 5, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var urls []string
			var hits []*atomic.Int64
			for i := range 3 {
				srv, n := statusBackend(t, fmt.Sprint(i), 503)
				urls, hits = append(urls, srv.URL), append(hits, n)
			}
			rt := &Route{Retry: retryPolicy(t, tt.attempts, "503")}
			h := newHandler(t, poolUpstream(urls...), rt, slog.New(slog.DiscardHandler))
			w := send(h, "GET", nil)
			if want := fmt.Sprintf("%d:0", tt.want-1); w.Code != 503 || w.Body.String() != want {
				t.Errorf("got %d %q, want 503 %q from the last backend tried", w.Code, w.Body, want)
			}
			if w.an.Attempts != tt.want {
				t.Errorf("%d attempts, want %d", w.an.Attempts, tt.want)
			}
			var total int64
			for i, n := range hits {
				if n.Load() > 1 {
					t.Errorf("backend %d tried %d times", i, n.Load())
				}
				total += n.Load()
			}
			if total != int64(tt.want) {
				t.Errorf("backends got %d requests, want %d", total, tt.want)
			}
			for i, b := range rt.Pool.Backends {
				if b.Inflight() != 0 {
					t.Errorf("backend %d has %d in flight after the request", i, b.Inflight())
				}
			}
		})
	}
}

func TestProxyRetryTimeout(t *testing.T) {
	slow := blockingBackend(t)
	ok, _ := statusBackend(t, "ok", 200)
	for _, tt := range []struct {
		method string
		want   int
	}{
		{"GET", 200},
		{"POST", 504},
	} {
		t.Run(tt.method, func(t *testing.T) {
			u := poolUpstream(slow.URL, ok.URL)
			u.Timeouts.ResponseHeader = 50 * time.Millisecond
			h := newHandler(t, u, &Route{Retry: retryPolicy(t, 1, "timeout")}, slog.New(slog.DiscardHandler))
			if w := send(h, tt.method, nil); w.Code != tt.want {
				t.Errorf("got %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// TestProxyRetryStopsOnClientCancel checks that a client that goes away is
// not retried for.
func TestProxyRetryStopsOnClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.WriteHeader(503)
	}))
	defer failing.Close()
	ok, okHits := statusBackend(t, "ok", 200)
	h := newHandler(t, poolUpstream(failing.URL, ok.URL), &Route{Retry: retryPolicy(t, 1, "503")}, slog.New(slog.DiscardHandler))
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if okHits.Load() != 0 || w.an.Attempts != 1 {
		t.Errorf("retried after the client left: %d attempts", w.an.Attempts)
	}
}

// TestProxyClientBodyFailed checks that a body the client cuts short while
// it is read for replay is the client's fault, not a backend's.
func TestProxyClientBodyFailed(t *testing.T) {
	backend, hits := statusBackend(t, "ok", 200)
	h := newHandler(t, testUpstream(backend.URL), &Route{Retry: retryPolicy(t, 1, "503")}, slog.New(slog.DiscardHandler))
	req := httptest.NewRequest("PUT", "/", strings.NewReader("short"))
	req.ContentLength = 100
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || w.an.Error != "client_body" || hits.Load() != 0 {
		t.Errorf("got %d, error %q, %d backend hits; want 400 client_body and none", w.Code, w.an.Error, hits.Load())
	}
}

// TestProxyRetryThroughServer checks a replayed body end to end over real
// connections, where the transport, not a recorder, reads it.
func TestProxyRetryThroughServer(t *testing.T) {
	failing, _ := statusBackend(t, "failing", 503)
	ok, _ := statusBackend(t, "ok", 200)
	front := serve(t, newHandler(t, poolUpstream(failing.URL, ok.URL), &Route{Retry: retryPolicy(t, 1, "503")}, slog.New(slog.DiscardHandler)))
	req, _ := http.NewRequest("PUT", front+"/", bytes.NewReader(make([]byte, 5000)))
	if resp, body := get(t, req); resp.StatusCode != 200 || body != "ok:5000" {
		t.Errorf("got %d %q, want 200 ok:5000", resp.StatusCode, body)
	}
}

// BenchmarkProxyReplayBody measures a PUT on a route with retries, which
// reads the body up front, against BenchmarkProxy's plain GET.
func BenchmarkProxyReplayBody(b *testing.B) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer backend.Close()
	p, _ := resilience.NewRetry(1, []string{"connect_error", "503"})
	front := proxyFor(b, backend, &Route{Retry: p})
	payload := make([]byte, 4096)
	b.ReportAllocs()
	for b.Loop() {
		req, _ := http.NewRequest("PUT", front+"/", bytes.NewReader(payload))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// TestProxyRetryBudget checks that once a route's retries use up its budget,
// failed requests go back to the client after one attempt, and the access
// log says why.
func TestProxyRetryBudget(t *testing.T) {
	a, aHits := statusBackend(t, "a", 503)
	b, bHits := statusBackend(t, "b", 503)
	p := retryPolicy(t, 1, "503")
	p.Budget = resilience.NewBudget(0, 1) // no share, a floor of 10 per window
	h := newHandler(t, poolUpstream(a.URL, b.URL), &Route{Retry: p}, slog.New(slog.DiscardHandler))
	for i := range 15 {
		w := send(h, "GET", nil)
		retried := i < 10
		if want := 1 + btoi(retried); w.an.Attempts != want || w.an.BudgetExhausted == retried {
			t.Errorf("request %d: %d attempts, budget exhausted %v", i, w.an.Attempts, w.an.BudgetExhausted)
		}
		if w.Code != 503 {
			t.Errorf("request %d got %d, want the backend's 503", i, w.Code)
		}
	}
	if got := aHits.Load() + bHits.Load(); got != 25 {
		t.Errorf("backends got %d requests, want 15 + 10 retries", got)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestProxyRetryBackoffCanceled checks that a retry waits out its backoff
// before going to the next backend, and that a client who leaves during
// the wait is answered as gone, without the retry being sent.
func TestProxyRetryBackoffCanceled(t *testing.T) {
	failing, failHits := statusBackend(t, "failing", 503)
	ok, okHits := statusBackend(t, "ok", 200)
	p := retryPolicy(t, 1, "503")
	p.Backoff = resilience.Backoff{Base: time.Hour, Max: time.Hour}
	h := newHandler(t, poolUpstream(failing.URL, ok.URL), &Route{Retry: p}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
		close(done)
	}()
	for deadline := time.Now().Add(5 * time.Second); failHits.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("first attempt never sent")
		}
	}
	time.Sleep(10 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("request finished without backing off")
	default:
	}
	cancel()
	<-done
	if okHits.Load() != 0 || w.an.Error != "client_closed" || w.Body.Len() != 0 {
		t.Errorf("after a cancel in backoff: %d retries sent, error %q, body %q", okHits.Load(), w.an.Error, w.Body)
	}
	if b := w.an.Route.Pool.Backends[1]; b.Inflight() != 0 {
		t.Errorf("next backend has %d in flight", b.Inflight())
	}
}
