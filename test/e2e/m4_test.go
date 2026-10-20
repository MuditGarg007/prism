package e2e

// The M4 exit tests: coalescing a cold stampede into one upstream request,
// hits, revalidation, stale-if-error, and the admin API's purge and stats,
// each through the real stack built from YAML.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MuditGarg007/prism/internal/admin"
)

// startCache runs prism with one caching route to backend, configured by
// cacheYAML, and its admin API. It returns the proxy's and the admin API's
// base URLs. The admin API is on "localhost" because validation refuses
// the listener's own address, even with port 0.
func startCache(t *testing.T, backend, cacheYAML string, logs io.Writer) (string, string) {
	t.Helper()
	srv := launch(t, fmt.Sprintf(`
admin: { listen: "localhost:0" }
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app, cache: %s }
`, backend, cacheYAML), logs)
	return "http://" + srv.Addrs()[0].String(), "http://" + srv.AdminAddr().String()
}

// fetch is a GET with header set that returns the status, X-Cache and body.
// It is safe to call off the test goroutine.
func fetch(client *http.Client, url string, header http.Header) (code int, xcache, body string, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, "", "", err
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Cache"), string(b), err
}

func cacheStats(t *testing.T, client *http.Client, adminURL string) admin.CacheStats {
	t.Helper()
	code, _, body, err := fetch(client, adminURL+"/cache/stats", nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("stats: %d %v %s", code, err, body)
	}
	var st admin.CacheStats
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestCacheStampede is the M4 exit test: 1,000 concurrent requests for a
// cold key make exactly one upstream request, and every client gets the
// full body. The backend holds its answer until all 999 other requests are
// waiting on it, which the admin API's stats report.
func TestCacheStampede(t *testing.T) {
	const clients = 1000
	var upstream atomic.Int64
	release := make(chan struct{})
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		<-release
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, strings.Repeat("x", 4096))
	})
	base, adminURL := startCache(t, backend.URL, "{ enabled: true, lock_timeout: 30s }", t.Output())
	client := newClient(t)
	client.Transport.(*http.Transport).MaxIdleConnsPerHost = clients

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		labels = map[string]int{}
	)
	for range clients {
		wg.Go(func() {
			code, xcache, body, err := fetch(client, base+"/cold", nil)
			if err != nil || code != http.StatusOK || len(body) != 4096 {
				t.Errorf("got %d, %d bytes, %v", code, len(body), err)
			}
			mu.Lock()
			labels[xcache]++
			mu.Unlock()
		})
	}
	// Released on failure too, so the deferred shutdown does not hang.
	func() {
		defer close(release)
		eventually(t, "every request to wait or lead", func() bool {
			return upstream.Load() == 1 && cacheStats(t, client, adminURL).Waiting == clients-1
		})
	}()
	wg.Wait()

	if n := upstream.Load(); n != 1 {
		t.Errorf("%d upstream requests, want 1", n)
	}
	if labels["MISS"] != 1 || labels["HIT"] != clients-1 {
		t.Errorf("X-Cache counts %v, want 1 MISS and %d HIT", labels, clients-1)
	}
	if st := cacheStats(t, client, adminURL); st.Miss != 1 || st.Hit != clients-1 || st.Waiting != 0 || st.Entries != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestCacheHit(t *testing.T) {
	var upstream atomic.Int64
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, "body of "+r.URL.Path)
	})
	logs := newLogBuffer(t)
	base, _ := startCache(t, backend.URL, "{ enabled: true }", logs)
	client := newClient(t)

	tests := []struct {
		header http.Header
		code   int
		xcache string
		body   string
	}{
		{nil, 200, "MISS", "body of /page"},
		{nil, 200, "HIT", "body of /page"},
		{http.Header{"If-None-Match": {`"v1"`}}, 304, "HIT", ""},
		{http.Header{"If-None-Match": {`"v0"`}}, 200, "HIT", "body of /page"},
	}
	for i, tt := range tests {
		code, xcache, body, err := fetch(client, base+"/page", tt.header)
		if err != nil || code != tt.code || xcache != tt.xcache || body != tt.body {
			t.Errorf("request %d: %d %s %q %v, want %d %s %q", i, code, xcache, body, err, tt.code, tt.xcache, tt.body)
		}
	}
	if n := upstream.Load(); n != 1 {
		t.Errorf("%d upstream requests, want 1", n)
	}
	for _, want := range []string{"cache=MISS", "cache=HIT"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("access log lacks %s", want)
		}
	}
}

// TestCacheRevalidate stores a response that is stale at once and checks
// that the next request revalidates it with the stored ETag, and that a 304
// refreshes the entry without resending the body.
func TestCacheRevalidate(t *testing.T) {
	var full, notModified atomic.Int64
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("Cache-Control", "max-age=0")
		io.WriteString(w, "versioned body")
	})
	base, _ := startCache(t, backend.URL, "{ enabled: true }", t.Output())
	client := newClient(t)

	for i, want := range []string{"MISS", "REVALIDATED", "HIT"} {
		code, xcache, body, err := fetch(client, base+"/doc", nil)
		if err != nil || code != 200 || xcache != want || body != "versioned body" {
			t.Errorf("request %d: %d %s %q %v, want 200 %s", i, code, xcache, body, err, want)
		}
	}
	if full.Load() != 1 || notModified.Load() != 1 {
		t.Errorf("backend sent %d full responses and %d 304s, want 1 and 1", full.Load(), notModified.Load())
	}
}

// TestCacheStaleIfError serves a stale entry while the backend answers 503,
// then while it is gone altogether.
func TestCacheStaleIfError(t *testing.T) {
	var failing atomic.Bool
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "max-age=0")
		io.WriteString(w, "last good body")
	})
	logs := newLogBuffer(t)
	base, adminURL := startCache(t, backend.URL, "{ enabled: true, stale_if_error: 1m }", logs)
	client := newClient(t)

	if _, xcache, _, err := fetch(client, base+"/s", nil); err != nil || xcache != "MISS" {
		t.Fatalf("first request: %s %v", xcache, err)
	}
	failing.Store(true)
	check := func(when string) {
		t.Helper()
		code, xcache, body, err := fetch(client, base+"/s", nil)
		if err != nil || code != 200 || xcache != "STALE" || body != "last good body" {
			t.Errorf("%s: %d %s %q %v, want 200 STALE", when, code, xcache, body, err)
		}
	}
	check("backend answering 503")
	backend.Close()
	check("backend gone")

	// Without stale-if-error the failure passes through.
	if code, xcache, _, err := fetch(client, base+"/never-stored", nil); err != nil || code != http.StatusBadGateway || xcache != "MISS" {
		t.Errorf("uncached path with backend gone: %d %s %v, want 502 MISS", code, xcache, err)
	}
	if st := cacheStats(t, client, adminURL); st.Stale != 2 {
		t.Errorf("stats = %+v, want 2 stale", st)
	}
	if !strings.Contains(logs.String(), "serving stale") {
		t.Error("no warning about serving stale")
	}
}

func TestCachePurge(t *testing.T) {
	var upstream atomic.Int64
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, r.URL.RequestURI())
	})
	base, adminURL := startCache(t, backend.URL, "{ enabled: true }", t.Output())
	client := newClient(t)
	host := strings.TrimPrefix(base, "http://")

	paths := []string{"/a", "/a?x=1", "/ab", "/b"}
	for _, p := range paths {
		if _, xcache, _, err := fetch(client, base+p, nil); err != nil || xcache != "MISS" {
			t.Fatalf("%s: %s %v", p, xcache, err)
		}
	}
	purge := func(body string) int {
		t.Helper()
		resp, err := client.Post(adminURL+"/cache/purge", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got admin.PurgeResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != 200 {
			t.Fatalf("purge %s: %d %v", body, resp.StatusCode, err)
		}
		return got.Purged
	}
	if n := purge(fmt.Sprintf(`{"host": %q, "path": "/a"}`, host)); n != 1 {
		t.Errorf("exact purge deleted %d, want 1", n)
	}
	if n := purge(fmt.Sprintf(`{"host": %q, "path": "/a", "prefix": true}`, strings.ToUpper(host))); n != 2 {
		t.Errorf("prefix purge deleted %d, want 2", n)
	}
	if st := cacheStats(t, client, adminURL); st.Entries != 1 || st.Miss != 4 {
		t.Errorf("stats = %+v, want 1 entry left and 4 misses", st)
	}

	want := map[string]string{"/a": "MISS", "/a?x=1": "MISS", "/ab": "MISS", "/b": "HIT"}
	for _, p := range paths {
		if _, xcache, _, err := fetch(client, base+p, nil); err != nil || xcache != want[p] {
			t.Errorf("%s after purge: %s %v, want %s", p, xcache, err, want[p])
		}
	}
	if n := upstream.Load(); n != 7 {
		t.Errorf("%d upstream requests, want 7", n)
	}
}
