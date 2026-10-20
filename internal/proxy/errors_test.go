package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/upstream"
)

// logBuffer collects log output written from server goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTestLogger() (*logBuffer, *slog.Logger) {
	buf := &logBuffer{}
	return buf, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// closedAddr returns an address that nothing listens on.
func closedAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// blockingBackend never answers until the test ends.
func blockingBackend(t *testing.T) *httptest.Server {
	block := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(backend.Close)
	t.Cleanup(func() { close(block) }) // runs first, so Close does not wait
	return backend
}

// roundTripErr sends a GET to url with tr and returns the error it must fail
// with.
func roundTripErr(t *testing.T, tr *http.Transport, url string) error {
	t.Helper()
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := tr.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("GET %s succeeded", url)
	}
	return err
}

func TestClassify(t *testing.T) {
	refused := roundTripErr(t, &http.Transport{}, "http://"+closedAddr(t))

	slow := blockingBackend(t)
	dialTimeout := roundTripErr(t, &http.Transport{
		DialContext: (&net.Dialer{Timeout: time.Nanosecond}).DialContext,
	}, slow.URL)
	headerTimeout := roundTripErr(t, &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}, slow.URL)

	// A backend that accepts and hangs up without answering.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	hangup := roundTripErr(t, &http.Transport{}, "http://"+ln.Addr().String())

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now())
	defer cancel2()
	<-expired.Done()

	tests := []struct {
		name       string
		ctx        context.Context
		err        error
		wantStatus int
		wantKind   errorKind
	}{
		{"connection refused", context.Background(), refused, 502, kindConnect},
		{"connect timeout", context.Background(), dialTimeout, 504, kindTimeout},
		{"response header timeout", context.Background(), headerTimeout, 504, kindTimeout},
		{"backend hung up", context.Background(), hangup, 502, kindUpstream},
		{"no backend", context.Background(), fmt.Errorf("pick: %w", upstream.ErrNoBackend), 503, kindNoBackend},
		{"other error", context.Background(), errors.New("boom"), 502, kindUpstream},
		{"body over max_body", context.Background(), fmt.Errorf("write: %w", &http.MaxBytesError{Limit: 10}), 413, kindBodyTooLarge},
		{"client canceled", canceled, context.Canceled, 499, kindClientClosed},
		{"client canceled during backend error", canceled, refused, 499, kindClientClosed},
		{"request deadline is not the client leaving", expired, context.DeadlineExceeded, 504, kindTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, kind := classify(tt.ctx, tt.err)
			if status != tt.wantStatus || kind != tt.wantKind {
				t.Errorf("classify(%v) = %d %s, want %d %s", tt.err, status, kind, tt.wantStatus, tt.wantKind)
			}
		})
	}
}

func TestProxyErrorStatus(t *testing.T) {
	slow := blockingBackend(t)
	tests := []struct {
		name       string
		backend    string
		setup      func(*Route)
		wantStatus int
		wantLog    string
	}{
		{"backend down", "http://" + closedAddr(t), nil, 502, "kind=connect"},
		{"slow headers", slow.URL, nil, 504, "kind=timeout"},
		{"no healthy backend", slow.URL, func(rt *Route) { rt.Pool.Backends[0].SetHealthy(false) }, 503, "kind=no_backend"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := testUpstream(tt.backend)
			u.Timeouts.ResponseHeader = 50 * time.Millisecond
			logs, log := newTestLogger()
			rt := &Route{}
			front := serve(t, newHandler(t, u, rt, log))
			if tt.setup != nil {
				tt.setup(rt)
			}

			req, _ := http.NewRequest("GET", front+"/x", nil)
			resp, body := get(t, req)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if want := http.StatusText(tt.wantStatus) + "\n"; body != want {
				t.Errorf("body %q, want %q", body, want)
			}
			out := logs.String()
			for _, want := range []string{"level=WARN", tt.wantLog, "upstream=test", "path=/x"} {
				if !strings.Contains(out, want) {
					t.Errorf("log %q does not contain %q", out, want)
				}
			}
		})
	}
}

func TestProxyClientCanceled(t *testing.T) {
	started := make(chan struct{})
	backendSawCancel := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(backendSawCancel)
	}))
	defer backend.Close()
	logs, log := newTestLogger()
	h := newHandler(t, testUpstream(backend.URL), &Route{}, log)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	<-started
	cancel()

	for _, ch := range []chan struct{}{done, backendSawCancel} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("cancellation did not reach the backend within 1s")
		}
	}
	// Nothing may be written for a client that is gone. The recorder's
	// Code stays at its default when WriteHeader is never called. The
	// request ID is set on arrival, but never sent.
	delete(rec.Header(), RequestIDHeader)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || len(rec.Header()) != 0 {
		t.Errorf("wrote %d %v %q to a gone client", rec.Code, rec.Header(), rec.Body)
	}
	if out := logs.String(); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "status=499") {
		t.Errorf("log %q, want a debug line with status=499", out)
	}
}

func TestProxyAbortLogs(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "only ten..")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer backend.Close()
	logs, log := newTestLogger()
	front := serve(t, newHandler(t, testUpstream(backend.URL), &Route{}, log))

	if resp, err := http.Get(front + "/"); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	out := logs.String()
	if !strings.Contains(out, "upstream response body failed") || !strings.Contains(out, "level=WARN") {
		t.Errorf("log %q, want a warning about the body", out)
	}
}
