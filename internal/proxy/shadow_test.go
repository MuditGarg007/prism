package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/balancer"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/limits"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// shadowSeen is a request as the shadow backend received it.
type shadowSeen struct {
	method, path, host, body, mark string
}

// shadowEnv is a route with a shadow, in front of a primary and a shadow
// backend.
type shadowEnv struct {
	h       *Handler
	rt      *Route
	front   string // URL of a real server for h
	seen    chan shadowSeen
	results chan observability.ShadowResult
}

// newShadowEnv builds the route, whose shadow backend runs shadow after
// recording what it got. rt may set other route fields.
func newShadowEnv(t *testing.T, rt *Route, primary, shadow http.HandlerFunc) *shadowEnv {
	t.Helper()
	env := &shadowEnv{
		seen:    make(chan shadowSeen, 100),
		results: make(chan observability.ShadowResult, 100),
	}
	ssrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		env.seen <- shadowSeen{r.Method, r.URL.Path, r.Host, string(b), r.Header.Get(ShadowHeader)}
		shadow(w, r)
	}))
	psrv := httptest.NewServer(primary)
	t.Cleanup(psrv.Close)
	pool, err := upstream.NewPool("shadow", testUpstream(ssrv.URL), balancer.New(config.BalancerRoundRobin), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last first: the shadow server closes, after waiting for
	// its requests, before its pool.
	t.Cleanup(pool.Close)
	t.Cleanup(ssrv.Close)
	if rt == nil {
		rt = &Route{}
	}
	rt.Shadow = &Shadow{
		Pool:    pool,
		Buckets: canaryBuckets,
		MaxBody: 1 << 10,
		Slots:   limits.NewSemaphore(4, 0),
		Timeout: 5 * time.Second,
		onDone:  func(r observability.ShadowResult) { env.results <- r },
	}
	env.rt = rt
	env.h = newHandler(t, testUpstream(psrv.URL), rt, slog.New(slog.DiscardHandler))
	env.front = serve(t, env.h)
	return env
}

func (env *shadowEnv) result(t *testing.T) observability.ShadowResult {
	t.Helper()
	select {
	case r := <-env.results:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no shadow result")
		return 0
	}
}

func (env *shadowEnv) send(t *testing.T, method, body string, chunked bool) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
		if chunked {
			rd = io.MultiReader(rd) // hides the length, so it is sent chunked
		}
	}
	req, _ := http.NewRequest(method, env.front+"/copy/me", rd)
	resp, _ := get(t, req)
	return resp
}

func ok(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	io.WriteString(w, "primary")
}

func TestProxyShadowCopies(t *testing.T) {
	for _, retry := range []bool{false, true} {
		var rt Route
		if retry {
			// The body is then read up front, and copied at once.
			r, err := resilience.NewRetry(1, []string{"connect_error"})
			if err != nil {
				t.Fatal(err)
			}
			rt.Retry = r
		}
		env := newShadowEnv(t, &rt, ok, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError) // thrown away
		})
		for _, tt := range []struct{ method, body string }{
			{"GET", ""},
			{"POST", "hello shadow"},
		} {
			resp := env.send(t, tt.method, tt.body, false)
			if resp.StatusCode != 200 {
				t.Errorf("retry=%v %s: primary got %d", retry, tt.method, resp.StatusCode)
			}
			got := <-env.seen
			want := shadowSeen{tt.method, "/copy/me", got.host, tt.body, "1"}
			if got != want {
				t.Errorf("retry=%v: shadow saw %+v, want %+v", retry, got, want)
			}
			if r := env.result(t); r != observability.ShadowSent {
				t.Errorf("retry=%v: result %v, want sent", retry, r)
			}
		}
	}
}

func TestProxyShadowSkipsBodies(t *testing.T) {
	env := newShadowEnv(t, nil, ok, ok)
	env.send(t, "POST", strings.Repeat("x", 2<<10), false) // over max_body
	if r := env.result(t); r != observability.ShadowSkipped {
		t.Errorf("large body: result %v, want body_skipped", r)
	}
	env.send(t, "POST", "short", true)
	if r := env.result(t); r != observability.ShadowSkipped {
		t.Errorf("chunked body: result %v, want body_skipped", r)
	}
	if len(env.seen) != 0 {
		t.Error("a skipped copy reached the shadow backend")
	}
}

// TestProxyShadowAddsNoLatency checks that a shadow backend that never
// answers neither delays nor fails the real requests, that copies past
// max_concurrency are dropped, and that a client leaving does not cancel
// its copy.
func TestProxyShadowAddsNoLatency(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // before the shadow server waits for its requests
	env := newShadowEnv(t, nil, ok, func(w http.ResponseWriter, r *http.Request) {
		// Longer than a client waits, so a copy that blocked its request
		// fails the test instead of hanging it.
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
	})
	env.rt.Shadow.Slots = limits.NewSemaphore(2, 0)

	for i := range 10 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "POST", env.front+"/", strings.NewReader("body"))
		start := time.Now()
		resp, body := get(t, req)
		cancel() // the client is gone; its copy must carry on
		if resp.StatusCode != 200 || body != "primary" {
			t.Fatalf("request %d: got %d %q", i, resp.StatusCode, body)
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("request %d took %v with the shadow stalled", i, d)
		}
	}
	// Two copies hold the slots, waiting on the stalled backend; the other
	// eight were dropped.
	for range 8 {
		if r := env.result(t); r != observability.ShadowBusy {
			t.Fatalf("result %v, want busy", r)
		}
	}
	if n := env.rt.Shadow.Slots.InUse(); n != 2 {
		t.Errorf("%d copies in flight, want 2", n)
	}
	unblock()
	for range 2 {
		if r := env.result(t); r != observability.ShadowSent {
			t.Errorf("released copy: result %v, want sent", r)
		}
	}
}

func TestProxyShadowTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	env := newShadowEnv(t, nil, ok, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	env.rt.Shadow.Timeout = 50 * time.Millisecond
	env.send(t, "GET", "", false)
	if r := env.result(t); r != observability.ShadowError {
		t.Errorf("result %v, want error", r)
	}
	// The slot comes back once the copy ends.
	deadline := time.Now().Add(2 * time.Second)
	for env.rt.Shadow.Slots.InUse() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the timed-out copy kept its slot")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestShadowCapture(t *testing.T) {
	c := &readCounter{r: strings.NewReader("hello world")}
	c.capture(11)
	buf := make([]byte, 5)
	c.Read(buf)
	if c.captured.Load() != nil {
		t.Fatal("published before the body was complete")
	}
	io.Copy(io.Discard, c)
	p := c.captured.Load()
	if p == nil || string(*p) != "hello world" {
		t.Fatalf("captured %v", p)
	}
}

func TestShadowSample(t *testing.T) {
	s := &Shadow{Buckets: 2500}
	n := 0
	for range 100000 {
		if s.sample() {
			n++
		}
	}
	if n < 24000 || n > 26000 {
		t.Errorf("25%%: sampled %d of 100000", n)
	}
	s.Buckets = canaryBuckets
	for range 1000 {
		if !s.sample() {
			t.Fatal("100% skipped a request")
		}
	}
}
