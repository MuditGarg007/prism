package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/cache"
	"github.com/MuditGarg007/prism/internal/limits"
)

// testClock is a fake clock that starts on a whole second, so a Date
// header written from it loses nothing to rounding and ages come out exact.
type testClock struct {
	start time.Time
	d     atomic.Int64
}

func newTestClock() *testClock { return &testClock{start: time.Now().Truncate(time.Second)} }

func (c *testClock) now() time.Time          { return c.start.Add(time.Duration(c.d.Load())) }
func (c *testClock) advance(d time.Duration) { c.d.Add(int64(d)) }

// cacheEnv is a caching route in front of one test backend.
type cacheEnv struct {
	h     *Handler
	c     *Cache
	clock *testClock
	hits  atomic.Int64 // requests that reached the backend
	last  atomic.Pointer[http.Request]
}

// newCacheEnv builds a handler whose one route caches, in front of a backend
// that counts requests and then runs backend. The backend's Date comes from
// the fake clock. rt may set other route fields; nil means none.
func newCacheEnv(t testing.TB, rt *Route, backend http.HandlerFunc) *cacheEnv {
	t.Helper()
	env := &cacheEnv{clock: newTestClock()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.hits.Add(1)
		env.last.Store(r)
		w.Header().Set("Date", env.clock.now().Format(http.TimeFormat))
		backend(w, r)
	}))
	t.Cleanup(srv.Close)
	if rt == nil {
		rt = &Route{}
	}
	env.c = &Cache{
		Store:       cache.NewStore(1 << 20),
		Flight:      cache.NewFlight(),
		Stats:       &cache.Stats{},
		MaxObject:   1 << 10,
		LockTimeout: 5 * time.Second,
		Now:         env.clock.now,
	}
	rt.Cache = env.c
	env.h = newHandler(t, testUpstream(srv.URL), rt, slog.New(slog.DiscardHandler))
	return env
}

// do serves one request for path on example.com, with the headers in hv as
// name, value pairs.
func (env *cacheEnv) do(method, path string, hv ...string) *annotatedRecorder {
	r := httptest.NewRequest(method, "http://example.com"+path, nil)
	for i := 0; i < len(hv); i += 2 {
		r.Header.Add(hv[i], hv[i+1])
	}
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	env.h.ServeHTTP(w, r)
	return w
}

// check fails t unless w got status, X-Cache xc and body, and the backend
// has seen hits requests in all.
func (env *cacheEnv) check(t *testing.T, w *annotatedRecorder, status int, xc, body string, hits int64) {
	t.Helper()
	if w.Code != status || w.Header().Get("X-Cache") != xc || w.Body.String() != body {
		t.Errorf("got %d, X-Cache %q, body %q; want %d, %q, %q",
			w.Code, w.Header().Get("X-Cache"), w.Body.String(), status, xc, body)
	}
	if w.an.Cache != xc {
		t.Errorf("annotated cache %q, want %q", w.an.Cache, xc)
	}
	if got := env.hits.Load(); got != hits {
		t.Errorf("backend saw %d requests, want %d", got, hits)
	}
}

// fixed answers every request with the given Cache-Control and body.
func fixed(cc, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, body)
	}
}

func TestCacheHit(t *testing.T) {
	env := newCacheEnv(t, nil, fixed("max-age=60", "hello"))
	env.check(t, env.do("GET", "/a"), 200, "MISS", "hello", 1)

	env.clock.advance(5 * time.Second)
	w := env.do("GET", "/a")
	env.check(t, w, 200, "HIT", "hello", 1)
	h := w.Header()
	if h.Get("Age") != "5" || h.Get("Content-Length") != "5" || h.Get("Content-Type") != "text/plain" {
		t.Errorf("hit: Age %q, Content-Length %q, Content-Type %q; want 5, 5, text/plain",
			h.Get("Age"), h.Get("Content-Length"), h.Get("Content-Type"))
	}

	// Other URLs are their own entries.
	env.check(t, env.do("GET", "/a?x=1"), 200, "MISS", "hello", 2)
	env.check(t, env.do("GET", "/b"), 200, "MISS", "hello", 3)

	env.clock.advance(time.Minute)
	env.check(t, env.do("GET", "/a"), 200, "MISS", "hello", 4) // expired, no validator

	if s := env.c.Stats.Snapshot(); s.Hit != 1 || s.Miss != 4 {
		t.Errorf("stats %+v, want 1 hit and 4 misses", s)
	}
}

func TestCacheNotStored(t *testing.T) {
	tests := []struct {
		name    string
		backend http.HandlerFunc
		req     []string
	}{
		{"no freshness", fixed("", "x"), nil},
		{"no-store", fixed("no-store, max-age=60", "x"), nil},
		{"private", fixed("private, max-age=60", "x"), nil},
		{"set-cookie", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Set-Cookie", "a=b")
			io.WriteString(w, "x")
		}, nil},
		{"status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusTeapot)
			io.WriteString(w, "x")
		}, nil},
		{"authorization", fixed("max-age=60", "x"), []string{"Authorization", "Bearer t"}},
		{"over max_object", fixed("max-age=60", strings.Repeat("x", 2000)), nil},
		{"chunked over max_object", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			for range 4 {
				io.WriteString(w, strings.Repeat("x", 500))
				w.(http.Flusher).Flush()
			}
		}, nil},
		{"trailers", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Trailer", "X-Sum")
			io.WriteString(w, "x")
			w.Header().Set("X-Sum", "1")
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newCacheEnv(t, nil, tt.backend)
			first := env.do("GET", "/", tt.req...)
			second := env.do("GET", "/", tt.req...)
			if env.hits.Load() != 2 || second.Header().Get("X-Cache") != "MISS" {
				t.Errorf("backend saw %d requests, second X-Cache %q; want 2, MISS",
					env.hits.Load(), second.Header().Get("X-Cache"))
			}
			if first.Body.Len() == 0 || first.Body.String() != second.Body.String() {
				t.Errorf("bodies %d and %d bytes, want the same, non-empty", first.Body.Len(), second.Body.Len())
			}
			if n := env.c.Store.Stats().Entries; n != 0 {
				t.Errorf("%d entries stored, want 0", n)
			}
		})
	}
}

func TestCacheAuthorizationPublic(t *testing.T) {
	env := newCacheEnv(t, nil, fixed("public, max-age=60", "x"))
	env.do("GET", "/", "Authorization", "Bearer t")
	env.check(t, env.do("GET", "/"), 200, "HIT", "x", 1)
}

func TestCacheUncacheableRequests(t *testing.T) {
	env := newCacheEnv(t, nil, fixed("max-age=60", "x"))
	env.do("GET", "/")
	for _, hv := range [][]string{
		{"Range", "bytes=0-0"},
		{"Cache-Control", "no-store"},
		{"If-Match", `"a"`},
		{"If-Unmodified-Since", "Tue, 20 Oct 2026 12:00:00 GMT"},
	} {
		before := env.hits.Load()
		w := env.do("GET", "/", hv...)
		if env.hits.Load() != before+1 || w.Header().Get("X-Cache") != "" {
			t.Errorf("%s: X-Cache %q, reached backend %v; want none, true",
				hv[0], w.Header().Get("X-Cache"), env.hits.Load() == before+1)
		}
	}
	// Other request directives are ignored.
	before := env.hits.Load()
	env.check(t, env.do("GET", "/", "Cache-Control", "no-cache"), 200, "HIT", "x", before)
}

func TestCacheResponseRulesOnServe(t *testing.T) {
	rt := &Route{Response: NewHeaderOps(map[string]string{"X-Served-By": "prism"}, []string{"Server-Timing"})}
	env := newCacheEnv(t, rt, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Server-Timing", "db;dur=3")
		io.WriteString(w, "x")
	})
	for _, xc := range []string{"MISS", "HIT"} {
		w := env.do("GET", "/")
		if w.Header().Get("X-Cache") != xc || w.Header().Get("X-Served-By") != "prism" || w.Header().Get("Server-Timing") != "" {
			t.Errorf("%s: headers %v", xc, w.Header())
		}
	}
	e, ok := env.c.Store.Get(cache.PrimaryKey(httptest.NewRequest("GET", "http://example.com/", nil)), http.Header{})
	if !ok {
		t.Fatal("nothing stored")
	}
	if e.Header.Get("Server-Timing") == "" || e.Header.Get("X-Served-By") != "" || e.Header.Get("X-Cache") != "" {
		t.Errorf("stored header %v, want the backend's, before the route's rules", e.Header)
	}
}

func TestCacheHead(t *testing.T) {
	env := newCacheEnv(t, nil, fixed("max-age=60", "hello"))
	env.check(t, env.do("HEAD", "/"), 200, "MISS", "", 1)
	env.check(t, env.do("HEAD", "/"), 200, "MISS", "", 2) // a HEAD miss is not stored
	env.check(t, env.do("GET", "/"), 200, "MISS", "hello", 3)
	w := env.do("HEAD", "/")
	env.check(t, w, 200, "HIT", "", 3)
	if w.Header().Get("Content-Length") != "5" {
		t.Errorf("HEAD hit Content-Length %q, want 5", w.Header().Get("Content-Length"))
	}
	if r := env.last.Load(); r.Method != "GET" {
		t.Errorf("last backend request %s, want GET", r.Method)
	}
}

func TestCacheBypass(t *testing.T) {
	var n atomic.Int64
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		fmt.Fprint(w, n.Add(1))
	})
	env.c.BypassHeader = "X-Bypass"
	env.check(t, env.do("GET", "/"), 200, "MISS", "1", 1)
	env.check(t, env.do("GET", "/", "X-Bypass", "1"), 200, "BYPASS", "2", 2)
	// The bypassed response was stored, so the next lookup sees it.
	env.check(t, env.do("GET", "/"), 200, "HIT", "2", 2)
	env.check(t, env.do("GET", "/", "X-Bypass", ""), 200, "BYPASS", "3", 3)
}

func TestCacheClientConditionals(t *testing.T) {
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Etag", `"v1"`)
		w.Header().Set("Last-Modified", "Tue, 20 Oct 2026 12:00:00 GMT")
		io.WriteString(w, "hello")
	})
	// On a miss, the backend sees no conditionals, so the full response can
	// be stored, and the client gets it.
	env.check(t, env.do("GET", "/", "If-None-Match", `"v1"`), 200, "MISS", "hello", 1)
	if r := env.last.Load(); r.Header.Get("If-None-Match") != "" {
		t.Errorf("backend got If-None-Match %q, want none", r.Header.Get("If-None-Match"))
	}

	w := env.do("GET", "/", "If-None-Match", `W/"v1"`)
	env.check(t, w, 304, "HIT", "", 1)
	h := w.Header()
	if h.Get("Etag") != `"v1"` || h.Get("Cache-Control") != "max-age=60" || h.Get("Content-Type") != "" || h.Get("Content-Length") != "" {
		t.Errorf("304 headers %v", h)
	}
	env.check(t, env.do("GET", "/", "If-None-Match", `"v2"`), 200, "HIT", "hello", 1)
	env.check(t, env.do("GET", "/", "If-Modified-Since", "Tue, 20 Oct 2026 12:00:00 GMT"), 304, "HIT", "", 1)
	env.check(t, env.do("GET", "/", "If-Modified-Since", "Tue, 20 Oct 2026 11:00:00 GMT"), 200, "HIT", "hello", 1)
}

func TestCacheVary(t *testing.T) {
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Vary", "Accept-Language")
		io.WriteString(w, r.Header.Get("Accept-Language"))
	})
	env.check(t, env.do("GET", "/", "Accept-Language", "en"), 200, "MISS", "en", 1)
	env.check(t, env.do("GET", "/", "Accept-Language", "fr"), 200, "MISS", "fr", 2)
	env.check(t, env.do("GET", "/", "Accept-Language", "en"), 200, "HIT", "en", 2)
	env.check(t, env.do("GET", "/", "Accept-Language", "fr"), 200, "HIT", "fr", 2)
	env.check(t, env.do("GET", "/"), 200, "MISS", "", 3)
}

// gate is a backend handler that blocks each request until open is closed.
func gate(open <-chan struct{}, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		<-open
		h(w, r)
	}
}

// waiting polls until n requests wait in env's Flight.
func (env *cacheEnv) waiting(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); env.c.Flight.Waiting() < n; {
		if time.Now().After(deadline) {
			t.Fatalf("%d requests waiting, want %d", env.c.Flight.Waiting(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// parallel serves n concurrent GETs for path and returns their recorders
// once all are done.
func (env *cacheEnv) parallel(n int, path string) []*annotatedRecorder {
	out := make([]*annotatedRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { out[i] = env.do("GET", path) })
	}
	wg.Wait()
	return out
}

// TestCacheCoalesces checks that 1,000 concurrent requests for a cold key
// reach the backend once, and that every one of them gets the response.
func TestCacheCoalesces(t *testing.T) {
	const n = 1000
	open := make(chan struct{})
	env := newCacheEnv(t, nil, gate(open, fixed("max-age=60", "hello")))
	done := make(chan []*annotatedRecorder)
	go func() { done <- env.parallel(n, "/") }()
	env.waiting(t, n-1)
	close(open)
	got := map[string]int{}
	for _, w := range <-done {
		if w.Code != 200 || w.Body.String() != "hello" {
			t.Fatalf("got %d %q", w.Code, w.Body.String())
		}
		got[w.Header().Get("X-Cache")]++
	}
	if env.hits.Load() != 1 || got["MISS"] != 1 || got["HIT"] != n-1 {
		t.Errorf("backend saw %d requests, X-Cache counts %v; want 1 and 1 MISS, %d HIT", env.hits.Load(), got, n-1)
	}
}

// TestCacheCoalescesUncacheable checks that waiters for a response that
// cannot be stored are released when its headers arrive, and that the key
// is then passed so later requests do not queue at all.
func TestCacheCoalescesUncacheable(t *testing.T) {
	open := make(chan struct{})
	var arrived atomic.Int64
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == 1 {
			<-open
		}
		io.WriteString(w, "x")
	})
	done := make(chan []*annotatedRecorder)
	go func() { done <- env.parallel(10, "/") }()
	env.waiting(t, 9)
	close(open)
	for _, w := range <-done {
		if w.Code != 200 || w.Header().Get("X-Cache") != "MISS" {
			t.Fatalf("got %d, X-Cache %q", w.Code, w.Header().Get("X-Cache"))
		}
	}
	if env.hits.Load() != 10 {
		t.Errorf("backend saw %d requests, want 10", env.hits.Load())
	}

	// Hit-for-pass: two requests in flight at once both reach the backend,
	// rather than one waiting for the other.
	both := make(chan struct{})
	var seen atomic.Int64
	env2 := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) > 1 {
			<-both
		}
		io.WriteString(w, "x")
	})
	env2.do("GET", "/")
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { env2.do("GET", "/") })
	}
	for env2.hits.Load() < 3 {
		if env2.c.Flight.Waiting() > 0 {
			t.Fatal("a request waited on a key marked uncacheable")
		}
		time.Sleep(time.Millisecond)
	}
	close(both)
	wg.Wait()
}

func TestCacheLockTimeout(t *testing.T) {
	open := make(chan struct{})
	var n atomic.Int64
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		if n.Add(1) == 1 {
			<-open
			io.WriteString(w, "leader")
			return
		}
		io.WriteString(w, "waiter")
	})
	env.c.LockTimeout = 20 * time.Millisecond
	leader := make(chan *annotatedRecorder)
	go func() { leader <- env.do("GET", "/") }()
	for env.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// The waiter gives up and fetches alone, storing nothing.
	env.check(t, env.do("GET", "/"), 200, "MISS", "waiter", 2)
	if env.c.Store.Stats().Entries != 0 {
		t.Error("a timed-out waiter stored its response")
	}
	close(open)
	if w := <-leader; w.Body.String() != "leader" {
		t.Errorf("leader got %q", w.Body.String())
	}
	env.check(t, env.do("GET", "/"), 200, "HIT", "leader", 2)
}

// TestCacheLeaderClientLeaves checks that the leader's fetch survives its
// client leaving, so the waiters still get the response.
func TestCacheLeaderClientLeaves(t *testing.T) {
	open := make(chan struct{})
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Length", "10")
		io.WriteString(w, "hello")
		w.(http.Flusher).Flush()
		<-open
		io.WriteString(w, "world")
	})
	front := serve(t, env.h)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", front+"/", nil)
	req.Host = "example.com" // the waiter's key
	leader := make(chan struct{})
	go func() {
		defer close(leader)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	for env.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waiter := make(chan *annotatedRecorder)
	go func() { waiter <- env.do("GET", "/") }()
	env.waiting(t, 1)
	cancel()
	<-leader
	close(open)
	env.check(t, <-waiter, 200, "HIT", "helloworld", 1)
}

// TestCacheHitsSkipConcurrencyLimit checks that a hit is served while every
// concurrency slot is taken, since hits put no load on a backend.
func TestCacheHitsSkipConcurrencyLimit(t *testing.T) {
	open := make(chan struct{})
	rt := &Route{Concurrency: limits.NewSemaphore(1, 0)}
	env := newCacheEnv(t, rt, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-open
		}
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, "x")
	})
	env.do("GET", "/fast")
	slow := make(chan *annotatedRecorder)
	go func() { slow <- env.do("GET", "/slow") }()
	for env.hits.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	env.check(t, env.do("GET", "/fast"), 200, "HIT", "x", 2)
	if w := env.do("GET", "/other"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("miss with every slot taken: %d, want 503", w.Code)
	}
	close(open)
	<-slow
}

// validating answers with an ETag and max-age=10, and with a 304 to a
// matching If-None-Match, counting the 304s.
func validating(body string, notModified *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=10")
		w.Header().Set("Etag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, body)
	}
}

func TestCacheRevalidates(t *testing.T) {
	var notModified atomic.Int64
	env := newCacheEnv(t, nil, validating("hello", &notModified))
	env.check(t, env.do("GET", "/"), 200, "MISS", "hello", 1)
	env.clock.advance(15 * time.Second)

	w := env.do("GET", "/")
	env.check(t, w, 200, "REVALIDATED", "hello", 2)
	if notModified.Load() != 1 {
		t.Errorf("backend sent %d 304s, want 1", notModified.Load())
	}
	if w.Header().Get("Age") != "0" || w.Header().Get("Content-Type") != "text/plain" || w.Header().Get("Content-Length") != "5" {
		t.Errorf("revalidated headers %v", w.Header())
	}
	// The merged entry is fresh again.
	env.clock.advance(5 * time.Second)
	env.check(t, env.do("GET", "/"), 200, "HIT", "hello", 2)

	// A client's own matching conditional gets a 304 from the merged entry.
	env.clock.advance(10 * time.Second)
	env.check(t, env.do("GET", "/", "If-None-Match", `"v1"`), 304, "REVALIDATED", "", 3)
	if s := env.c.Stats.Snapshot(); s.Revalidated != 2 {
		t.Errorf("stats %+v, want 2 revalidated", s)
	}
}

func TestCacheRevalidatesLastModified(t *testing.T) {
	const lm = "Tue, 20 Oct 2026 12:00:00 GMT"
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=10")
		w.Header().Set("Last-Modified", lm)
		if r.Header.Get("If-Modified-Since") == lm {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		io.WriteString(w, "hello")
	})
	env.do("GET", "/")
	env.clock.advance(15 * time.Second)
	env.check(t, env.do("GET", "/"), 200, "REVALIDATED", "hello", 2)
}

// TestCacheRevalidationReplaced checks a revalidation answered with a new
// full response, cacheable or not.
func TestCacheRevalidationReplaced(t *testing.T) {
	var version atomic.Int64
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		v := version.Load()
		if v < 2 {
			w.Header().Set("Cache-Control", "max-age=10")
		}
		w.Header().Set("Etag", fmt.Sprintf(`"v%d"`, v))
		fmt.Fprintf(w, "v%d", v)
	})
	env.do("GET", "/")
	version.Store(1)
	env.clock.advance(15 * time.Second)
	env.check(t, env.do("GET", "/"), 200, "MISS", "v1", 2)
	if r := env.last.Load(); r.Header.Get("If-None-Match") != `"v0"` {
		t.Errorf("revalidation sent If-None-Match %q, want the stored ETag", r.Header.Get("If-None-Match"))
	}
	env.check(t, env.do("GET", "/"), 200, "HIT", "v1", 2)

	// The new response cannot be stored, so the stale entry goes too.
	version.Store(2)
	env.clock.advance(15 * time.Second)
	env.check(t, env.do("GET", "/"), 200, "MISS", "v2", 3)
	if n := env.c.Store.Stats().Entries; n != 0 {
		t.Errorf("%d entries left, want 0", n)
	}
}

func TestCacheStaleIfError(t *testing.T) {
	var fail atomic.Bool
	tests := []struct {
		name   string
		cc     string
		window time.Duration
		after  time.Duration // past freshness
		stale  bool
	}{
		{"within the route's window", "max-age=10", time.Minute, 30 * time.Second, true},
		{"past the route's window", "max-age=10", time.Minute, 2 * time.Minute, false},
		{"route window off", "max-age=10", 0, time.Second, false},
		{"narrowed by the response", "max-age=10, stale-if-error=5", time.Minute, 30 * time.Second, false},
		{"within the response's window", "max-age=10, stale-if-error=60", time.Minute, 30 * time.Second, true},
		{"not widened by the response", "max-age=10, stale-if-error=600", time.Minute, 2 * time.Minute, false},
		{"must-revalidate", "max-age=10, must-revalidate", time.Minute, time.Second, false},
		{"s-maxage", "s-maxage=10", time.Minute, time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fail.Store(false)
			env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					io.WriteString(w, "down")
					return
				}
				w.Header().Set("Cache-Control", tt.cc)
				io.WriteString(w, "hello")
			})
			env.c.StaleIfError = tt.window
			env.do("GET", "/")
			fail.Store(true)
			env.clock.advance(10*time.Second + tt.after)
			w := env.do("GET", "/")
			if tt.stale {
				env.check(t, w, 200, "STALE", "hello", 2)
				if w.an.Error != string(kindUpstream) {
					t.Errorf("annotated error %q, want %q", w.an.Error, kindUpstream)
				}
			} else {
				env.check(t, w, 503, "MISS", "down", 2)
				if env.c.Store.Stats().Entries != 1 {
					t.Error("a failed backend removed the stale entry")
				}
			}
		})
	}
}

func TestCacheStaleIfConnectError(t *testing.T) {
	env := newCacheEnv(t, nil, fixed("max-age=10", "hello"))
	env.c.StaleIfError = time.Minute
	env.do("GET", "/")
	env.clock.advance(20 * time.Second)
	// Point the pool at a closed port.
	h := newHandler(t, testUpstream("http://"+closedAddr(t)), &Route{Cache: env.c}, slog.New(slog.DiscardHandler))
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com/", nil))
	if w.Code != 200 || w.Header().Get("X-Cache") != "STALE" || w.Body.String() != "hello" || w.an.Error != string(kindConnect) {
		t.Errorf("got %d, X-Cache %q, body %q, error %q; want 200, STALE, hello, connect",
			w.Code, w.Header().Get("X-Cache"), w.Body.String(), w.an.Error)
	}
	if w.Header().Get("Age") != "20" {
		t.Errorf("Age %q, want 20", w.Header().Get("Age"))
	}
}

// TestCacheStaleWaiters checks that when the leader finds the backend
// failing, its waiters serve the stale entry rather than each sending the
// failing backend a request of its own.
func TestCacheStaleWaiters(t *testing.T) {
	const n = 50
	open := make(chan struct{})
	var fail atomic.Bool
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			<-open
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Cache-Control", "max-age=10")
		io.WriteString(w, "hello")
	})
	env.c.StaleIfError = time.Minute
	env.do("GET", "/")
	fail.Store(true)
	env.clock.advance(20 * time.Second)
	done := make(chan []*annotatedRecorder)
	go func() { done <- env.parallel(n, "/") }()
	env.waiting(t, n-1)
	close(open)
	for _, w := range <-done {
		if w.Code != 200 || w.Header().Get("X-Cache") != "STALE" || w.Body.String() != "hello" {
			t.Fatalf("got %d, X-Cache %q, body %q", w.Code, w.Header().Get("X-Cache"), w.Body.String())
		}
	}
	if env.hits.Load() != 2 {
		t.Errorf("backend saw %d requests, want 2", env.hits.Load())
	}
}

// TestCacheWaitersShareFreshlyStale checks that an entry stored while
// requests waited answers them even with max-age=0, instead of each one
// revalidating in turn.
func TestCacheWaitersShareFreshlyStale(t *testing.T) {
	const n = 20
	open := make(chan struct{})
	env := newCacheEnv(t, nil, gate(open, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("Etag", `"v1"`)
		io.WriteString(w, "hello")
	}))
	done := make(chan []*annotatedRecorder)
	go func() { done <- env.parallel(n, "/") }()
	env.waiting(t, n-1)
	close(open)
	<-done
	if env.hits.Load() != 1 {
		t.Errorf("backend saw %d requests, want 1", env.hits.Load())
	}
}

func BenchmarkCacheHit(b *testing.B) {
	env := newCacheEnv(b, nil, fixed("max-age=3600", strings.Repeat("x", 1024)))
	r := httptest.NewRequest("GET", "http://example.com/a", nil)
	w := httptest.NewRecorder()
	env.h.ServeHTTP(w, r)
	b.ReportAllocs()
	for b.Loop() {
		w := discardWriter{h: http.Header{}}
		env.h.ServeHTTP(&w, r)
		if w.code != 200 {
			b.Fatal(w.code)
		}
	}
	if env.hits.Load() != 1 {
		b.Fatalf("backend saw %d requests", env.hits.Load())
	}
}

// discardWriter is a ResponseWriter that keeps only the status, so a
// benchmark measures the handler rather than a recorder.
type discardWriter struct {
	h    http.Header
	code int
}

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(code int)        { w.code = code }
