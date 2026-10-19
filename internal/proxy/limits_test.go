package proxy

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/limits"
	"github.com/MuditGarg007/prism/internal/ratelimit"
)

// annotatedRecorder is a ResponseRecorder that collects Annotations, as the
// access log's writer does.
type annotatedRecorder struct {
	*httptest.ResponseRecorder
	an Annotations
}

func (a *annotatedRecorder) Annotations() *Annotations { return &a.an }

// countingBackend counts the requests that reach it.
func countingBackend(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestProxyRateLimit(t *testing.T) {
	backend, hits := countingBackend(t)
	trusted := netip.MustParsePrefix("127.0.0.1/32")
	h := newHandler(t, testUpstream(backend.URL), &Route{
		RateLimit: ratelimit.New(1, time.Minute, 2),
	}, slog.New(slog.DiscardHandler), trusted)

	send := func(client string) *annotatedRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Forwarded-For", client)
		w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, req)
		return w
	}
	for i := range 2 {
		if w := send("198.51.100.1"); w.Code != http.StatusOK {
			t.Fatalf("request %d within the burst got %d", i, w.Code)
		}
	}
	w := send("198.51.100.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request over the burst got %d, want 429", w.Code)
	}
	// The bucket refills one request a minute.
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if w.an.Error != "rate_limited" || w.an.Route == nil || w.an.ClientIP.String() != "198.51.100.1" {
		t.Errorf("annotations = %+v", w.an)
	}
	// The client IP behind a trusted proxy is the key, not the proxy.
	if w := send("198.51.100.2"); w.Code != http.StatusOK {
		t.Errorf("another client got %d, want 200", w.Code)
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("backend got %d requests, want 3: rejected ones must not reach it", n)
	}
}

// TestProxyRateLimitIgnoresSpoofedXFF checks that an untrusted client cannot
// get a fresh bucket by sending a different X-Forwarded-For each time.
func TestProxyRateLimitIgnoresSpoofedXFF(t *testing.T) {
	backend, _ := countingBackend(t)
	front := proxyFor(t, backend, &Route{RateLimit: ratelimit.New(1, time.Minute, 1)})
	for i, xff := range []string{"203.0.113.1", "203.0.113.2"} {
		req, _ := http.NewRequest("GET", front+"/", nil)
		req.Header.Set("X-Forwarded-For", xff)
		resp, _ := get(t, req)
		if want := []int{200, 429}[i]; resp.StatusCode != want {
			t.Errorf("request %d with X-Forwarded-For %s got %d, want %d", i, xff, resp.StatusCode, want)
		}
	}
}

// readingBackend reads the whole request body and reports how many bytes it
// got, counting the requests that reach it.
func readingBackend(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		io.WriteString(w, strconv.FormatInt(n, 10))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestProxyMaxBody(t *testing.T) {
	const limit = 1000
	backend, hits := readingBackend(t)
	front := proxyFor(t, backend, &Route{MaxBody: limit})
	body := func(n int, chunked bool) io.Reader {
		r := bytes.NewReader(bytes.Repeat([]byte("x"), n))
		if chunked {
			// MultiReader hides the length, so the client sends it chunked.
			return io.MultiReader(r)
		}
		return r
	}
	tests := []struct {
		name     string
		size     int
		chunked  bool
		want     int
		mayReach bool // whether the request may reach the backend
	}{
		{"length at the limit", limit, false, 200, true},
		{"length over the limit", limit + 1, false, 413, false},
		{"chunked at the limit", limit, true, 200, true},
		{"chunked over the limit", limit + 1, true, 413, true},
		{"chunked far over the limit", 1 << 20, true, 413, true},
		{"no body", 0, false, 200, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := hits.Load()
			req, _ := http.NewRequest("POST", front+"/", body(tt.size, tt.chunked))
			resp, got := get(t, req)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.want == 200 && got != strconv.Itoa(tt.size) {
				t.Errorf("backend read %s bytes, want %d", got, tt.size)
			}
			if tt.want == 413 && !resp.Close {
				t.Error("413 did not close the connection, which still holds unread body")
			}
			if !tt.mayReach && hits.Load() != before {
				t.Error("a body declared over the limit reached the backend")
			}
		})
	}
}

// TestProxyMaxBodyAnnotations checks the kind recorded for both ways a body
// can be too large, and that a mid-stream one is logged at debug, not warn.
func TestProxyMaxBodyAnnotations(t *testing.T) {
	backend, _ := readingBackend(t)
	logs, log := newTestLogger()
	h := newHandler(t, testUpstream(backend.URL), &Route{MaxBody: 10}, log)
	for name, body := range map[string]io.Reader{
		"length":  strings.NewReader("01234567890"),
		"chunked": io.MultiReader(strings.NewReader("01234567890")),
	} {
		t.Run(name, func(t *testing.T) {
			w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, httptest.NewRequest("POST", "/", body))
			if w.Code != http.StatusRequestEntityTooLarge || w.an.Error != "body_too_large" {
				t.Errorf("status %d, error %q; want 413 body_too_large", w.Code, w.an.Error)
			}
			if w.Header().Get("Retry-After") != "" {
				t.Error("413 has a Retry-After, but waiting will not help")
			}
		})
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("a client's oversized body was logged as a warning: %s", out)
	}
}

// TestProxyMaxBodyNoContinue checks that a client asking to upload a body
// over the limit gets 413 instead of "100 Continue".
func TestProxyMaxBodyNoContinue(t *testing.T) {
	backend, hits := readingBackend(t)
	front := proxyFor(t, backend, &Route{MaxBody: 1000})
	conn, err := net.Dial("tcp", strings.TrimPrefix(front, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5000\r\nExpect: 100-continue\r\n\r\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 413 ") {
		t.Errorf("first response line %q, want a 413", line)
	}
	if hits.Load() != 0 {
		t.Error("request reached the backend")
	}
}

// streamingBackend sends its headers at once and finishes the body only when
// release is closed, so a request to it stays in flight.
func streamingBackend(t *testing.T) (srv *httptest.Server, release chan struct{}) {
	release = make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "start ")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "end")
	}))
	t.Cleanup(srv.Close)
	return srv, release
}

// waitIdle waits for every slot of s to be given back.
func waitIdle(t *testing.T, s *limits.Semaphore) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); s.InUse() != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("%d slots still held", s.InUse())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProxyConcurrencyLimit(t *testing.T) {
	backend, release := streamingBackend(t)
	sem := limits.NewSemaphore(2)
	h := newHandler(t, testUpstream(backend.URL), &Route{Concurrency: sem}, slog.New(slog.DiscardHandler))
	front := serve(t, h)

	// Two requests take both slots. Their headers have arrived, but their
	// bodies have not, so the slots must still be held.
	var open []*http.Response
	for range 2 {
		resp, err := http.Get(front + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		open = append(open, resp)
	}
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("request over the cap got %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if w.an.Error != "overloaded" || w.an.Backend != nil {
		t.Errorf("annotations = %+v, want overloaded and no backend", w.an)
	}

	close(release)
	for _, resp := range open {
		if b, _ := io.ReadAll(resp.Body); string(b) != "start end" {
			t.Errorf("held response body %q", b)
		}
	}
	waitIdle(t, sem)
	req, _ := http.NewRequest("GET", front+"/", nil)
	if resp, _ := get(t, req); resp.StatusCode != http.StatusOK {
		t.Errorf("request after the slots freed got %d", resp.StatusCode)
	}
}

// TestProxyConcurrencyReleasedOnError checks that every way forwarding can
// fail gives the slot back, including an abort that panics.
func TestProxyConcurrencyReleasedOnError(t *testing.T) {
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "short")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer truncated.Close()
	for name, url := range map[string]string{
		"backend down":   "http://" + closedAddr(t),
		"truncated body": truncated.URL,
	} {
		t.Run(name, func(t *testing.T) {
			sem := limits.NewSemaphore(1)
			h := newHandler(t, testUpstream(url), &Route{Concurrency: sem}, slog.New(slog.DiscardHandler))
			func() {
				defer func() { recover() }()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
			}()
			if sem.InUse() != 0 {
				t.Errorf("%d slots held after the request failed", sem.InUse())
			}
		})
	}
}

// TestProxyLimitOrder checks the pipeline order: the rate limit first, then
// the body size, then the concurrency cap, so a request refused early never
// takes a slot.
func TestProxyLimitOrder(t *testing.T) {
	backend, _ := countingBackend(t)
	sem := limits.NewSemaphore(1)
	h := newHandler(t, testUpstream(backend.URL), &Route{
		RateLimit:   ratelimit.New(1, time.Minute, 1),
		MaxBody:     1,
		Concurrency: sem,
	}, slog.New(slog.DiscardHandler))
	sem.TryAcquire() // the route is at its cap
	for _, want := range []int{413, 429} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader("too big")))
		if w.Code != want {
			t.Errorf("got %d, want %d", w.Code, want)
		}
	}
	if sem.InUse() != 1 {
		t.Errorf("refused requests changed the slots in use to %d", sem.InUse())
	}
}
