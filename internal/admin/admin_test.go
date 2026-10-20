package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/runtime"
)

var discard = slog.New(slog.DiscardHandler)

// cachingRuntime builds a runtime whose only route caches everything from
// a backend that answers with the request URI.
func cachingRuntime(t *testing.T, enabled bool) *runtime.Runtime {
	t.Helper()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, r.RequestURI)
	}))
	t.Cleanup(b.Close)
	cfg, err := config.Parse(fmt.Appendf(nil, `
listeners: [{ addr: ":8080" }]
cache: { max_size: 2MB }
upstreams:
  web: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: web, cache: { enabled: %t } }
`, b.URL, enabled), "")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.Build(cfg, discard, runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	return rt
}

// fill stores a response for each URL through rt's handler.
func fill(t *testing.T, rt *runtime.Runtime, urls ...string) {
	t.Helper()
	for _, u := range urls {
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if got := rec.Header().Get("X-Cache"); got != "MISS" {
			t.Fatalf("%s: X-Cache %q, want MISS", u, got)
		}
	}
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestCacheStats(t *testing.T) {
	rt := cachingRuntime(t, true)
	h := New(runtime.NewCurrent(rt), nil, nil, discard)
	fill(t, rt, "http://a.test/x", "http://a.test/y")
	rec := httptest.NewRecorder()
	rt.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://a.test/x", nil))

	rec = do(h, "GET", "/cache/stats", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var st CacheStats
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Entries != 2 || st.Bytes <= 0 || st.MaxBytes != 2<<20 || st.Hit != 1 || st.Miss != 2 {
		t.Errorf("stats = %+v", st)
	}
	for _, field := range []string{`"entries"`, `"max_bytes"`, `"evictions"`, `"waiting"`, `"revalidated"`, `"bypass"`} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("body %s lacks %s", rec.Body, field)
		}
	}
}

func TestCachePurge(t *testing.T) {
	tests := []struct {
		body string
		want int
	}{
		{`{"host": "a.test", "path": "/x"}`, 1},
		{`{"host": "A.Test:80", "path": "/x"}`, 1},
		{`{"host": "a.test:443", "path": "/x"}`, 1},
		{`{"host": "a.test", "path": "/x?q=1"}`, 1},
		{`{"host": "a.test", "path": "/x", "prefix": true}`, 3},
		{`{"host": "a.test", "path": "/", "prefix": true}`, 4},
		{`{"host": "a.test", "path": "/nothing"}`, 0},
		{`{"host": "a.test:8080", "path": "/x"}`, 0},
		{`{"host": "b.test", "path": "/x"}`, 1},
	}
	for _, tt := range tests {
		rt := cachingRuntime(t, true)
		h := New(runtime.NewCurrent(rt), nil, nil, discard)
		fill(t, rt, "http://a.test/x", "http://a.test/x?q=1", "http://a.test/xyz", "http://a.test/y", "http://b.test/x")

		rec := do(h, "POST", "/cache/purge", tt.body)
		var got PurgeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
			t.Errorf("%s: status %d, body %s", tt.body, rec.Code, rec.Body)
			continue
		}
		if got.Purged != tt.want {
			t.Errorf("%s: purged %d, want %d", tt.body, got.Purged, tt.want)
		}
		if n := rt.Cache.Store.Stats().Entries; n != 5-tt.want {
			t.Errorf("%s: %d entries left, want %d", tt.body, n, 5-tt.want)
		}
	}
}

func TestCachePurgeInvalid(t *testing.T) {
	rt := cachingRuntime(t, true)
	h := New(runtime.NewCurrent(rt), nil, nil, discard)
	fill(t, rt, "http://a.test/x")
	for _, body := range []string{
		``,
		`not json`,
		`{"host": "a.test"}`,
		`{"path": "/x"}`,
		`{"host": "a.test", "path": ""}`,
		`{"host": "a.test", "path": "x", "prefix": true}`,
		`{"host": "a.test", "path": "/x", "unknown": 1}`,
		`{"host": "a.test", "path": "/x"} {}`,
		`{"host": "a.test", "path": "/x", "prefix": "yes"}`,
		`{"host": "a.test", "path": "/` + strings.Repeat("x", maxBody) + `"}`,
	} {
		rec := do(h, "POST", "/cache/purge", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%.60s: status %d, body %.200s", body, rec.Code, rec.Body)
		}
	}
	if n := rt.Cache.Store.Stats().Entries; n != 1 {
		t.Errorf("invalid purges deleted entries: %d left", n)
	}
}

func TestRouting(t *testing.T) {
	h := New(runtime.NewCurrent(cachingRuntime(t, true)), nil, nil, discard)
	tests := []struct {
		method, path string
		code         int
	}{
		{"POST", "/cache/stats", http.StatusMethodNotAllowed},
		{"GET", "/cache/purge", http.StatusMethodNotAllowed},
		{"GET", "/nothing", http.StatusNotFound},
		{"POST", "/routes", http.StatusMethodNotAllowed},
		{"DELETE", "/upstreams", http.StatusMethodNotAllowed},
		{"PUT", "/config", http.StatusMethodNotAllowed},
		{"GET", "/config", http.StatusOK},
	}
	for _, tt := range tests {
		rec := do(h, tt.method, tt.path, "")
		if rec.Code != tt.code {
			t.Errorf("%s %s: status %d, want %d", tt.method, tt.path, rec.Code, tt.code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: no Cache-Control: no-store", tt.method, tt.path)
		}
	}
}

func TestNoCache(t *testing.T) {
	h := New(runtime.NewCurrent(cachingRuntime(t, false)), nil, nil, discard)
	for _, req := range []struct{ method, path, body string }{
		{"GET", "/cache/stats", ""},
		{"POST", "/cache/purge", `{"host": "a.test", "path": "/x"}`},
	} {
		if rec := do(h, req.method, req.path, req.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", req.method, req.path, rec.Code)
		}
	}
}

// TestReload checks that the admin API follows the current runtime rather
// than the one it was created with.
func TestReload(t *testing.T) {
	a, b := cachingRuntime(t, true), cachingRuntime(t, true)
	cur := runtime.NewCurrent(a)
	h := New(cur, nil, nil, discard)
	fill(t, b, "http://a.test/x")
	cur.Swap(b)
	rec := do(h, "POST", "/cache/purge", `{"host": "a.test", "path": "/x"}`)
	if !strings.Contains(rec.Body.String(), `"purged":1`) {
		t.Errorf("purge after reload: %s", rec.Body)
	}
}

// TestReloadEndpoint checks POST /reload's answers, and that it exists only
// with a reloader.
func TestReloadEndpoint(t *testing.T) {
	cur := runtime.NewCurrent(cachingRuntime(t, false))
	var err error
	calls := 0
	h := New(cur, nil, func() error { calls++; return err }, discard)

	rec := do(h, "POST", "/reload", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"reloaded":true}` {
		t.Errorf("success: %d %s", rec.Code, rec.Body)
	}
	err = errors.New("routes[0]: unknown upstream")
	rec = do(h, "POST", "/reload", "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "reload failed: routes[0]: unknown upstream") {
		t.Errorf("failure: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/reload", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	if calls != 2 {
		t.Errorf("reload called %d times, want 2", calls)
	}
	if rec := do(New(cur, nil, nil, discard), "POST", "/reload", ""); rec.Code != http.StatusNotFound {
		t.Errorf("without a reloader: %d, want 404", rec.Code)
	}
}
