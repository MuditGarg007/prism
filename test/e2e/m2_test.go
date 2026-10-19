package e2e

// The M2 exit tests: rate limits, body and concurrency limits, retries and
// ip_hash, each through the real stack built from YAML.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// send sends a request with an optional X-Forwarded-For and returns the
// response with its body read.
func send(t *testing.T, client *http.Client, method, url, xff string, body io.Reader) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(b)
}

// deadURL returns the URL of a loopback port that nothing listens on, so a
// connection to it is refused.
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

func TestRateLimit(t *testing.T) {
	var hits atomic.Int64
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	})
	// One request a minute with a burst of 2: the third request in a row
	// must wait a full interval.
	base := startProxyYAML(t, fmt.Sprintf(`trusted_proxies: ["127.0.0.1"]
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - match: { path_prefix: / }
    upstream: app
    rate_limit: { per_ip: "1/m", burst: 2 }
`, backend.URL))
	client := newClient(t)

	for i := range 2 {
		if resp, _ := send(t, client, http.MethodGet, base+"/", "1.2.3.4", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, resp.StatusCode)
		}
	}
	resp, _ := send(t, client, http.MethodGet, base+"/", "1.2.3.4", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third request: status %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After %q, want 60", got)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("backend saw %d requests, want 2", n)
	}

	// Another client behind the trusted proxy has its own bucket.
	if resp, _ := send(t, client, http.MethodGet, base+"/", "5.6.7.8", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("other client: status %d, want 200", resp.StatusCode)
	}
}

func TestMaxBody(t *testing.T) {
	var hits atomic.Int64
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, n)
	})
	base := startProxyYAML(t, fmt.Sprintf(`upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app, max_body: 1KB }
`, backend.URL))

	tests := []struct {
		name       string
		size       int
		chunked    bool
		wantStatus int
	}{
		{"declared at the limit", 1024, false, http.StatusOK},
		{"declared over the limit", 1025, false, http.StatusRequestEntityTooLarge},
		{"chunked at the limit", 1024, true, http.StatusOK},
		{"chunked over the limit", 64 << 10, true, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A 413 closes the connection, so each case gets its own client.
			client := newClient(t)
			hits.Store(0)
			var body io.Reader = strings.NewReader(strings.Repeat("x", tt.size))
			if tt.chunked {
				body = io.MultiReader(body) // hides the length
			}
			resp, got := send(t, client, http.MethodPost, base+"/", "", body)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			switch {
			case tt.wantStatus == http.StatusOK:
				if got != fmt.Sprint(tt.size) {
					t.Errorf("backend read %s bytes, want %d", got, tt.size)
				}
			case !tt.chunked:
				// A declared oversize is refused before anything is sent on.
				if n := hits.Load(); n != 0 {
					t.Errorf("backend saw %d requests, want 0", n)
				}
			}
		})
	}
}

func TestConcurrencyCap(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	})
	base := startProxyYAML(t, fmt.Sprintf(`upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app, max_concurrency: 1 }
`, backend.URL))
	// Runs before the proxy's shutdown, which would otherwise wait on the
	// blocked request.
	t.Cleanup(unblock)
	client := newClient(t)

	done := make(chan int, 1)
	go func() {
		resp, err := client.Get(base + "/slow")
		if err != nil {
			t.Errorf("slow request: %v", err)
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request never reached the backend")
	}

	resp, _ := send(t, client, http.MethodGet, base+"/fast", "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("request at the cap: status %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After %q, want 1", got)
	}

	unblock()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("slow request: status %d, want 200", code)
	}
	if resp, _ := send(t, client, http.MethodGet, base+"/fast", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("request after the slot came back: status %d, want 200", resp.StatusCode)
	}
}

func TestRetrySkipsDeadBackend(t *testing.T) {
	var hits atomic.Int64
	live := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Write(b)
	})
	// Round robin sends every other first attempt to the dead backend.
	base := startProxyYAML(t, fmt.Sprintf(`upstreams:
  app: { backends: [{ url: %q }, { url: %q }] }
routes:
  - match: { path_prefix: / }
    upstream: app
    retry: { attempts: 1, on: [connect_error] }
`, deadURL(t), live.URL))
	client := newClient(t)

	bodies := []struct {
		name   string
		method string
		body   string
		hide   bool // hide the length, so the body is sent chunked
	}{
		{"GET", http.MethodGet, "", false},
		{"POST", http.MethodPost, "hello", false},
		{"chunked POST", http.MethodPost, "hello", true},
	}
	const n = 4
	for _, b := range bodies {
		t.Run(b.name, func(t *testing.T) {
			hits.Store(0)
			for i := range n {
				var body io.Reader
				if b.body != "" {
					body = strings.NewReader(b.body)
					if b.hide {
						body = io.MultiReader(body)
					}
				}
				resp, got := send(t, client, b.method, base+"/", "", body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("request %d: status %d, want 200", i, resp.StatusCode)
				}
				if got != b.body {
					t.Errorf("request %d: backend echoed %q, want %q", i, got, b.body)
				}
			}
			if got := hits.Load(); got != n {
				t.Errorf("live backend saw %d requests, want %d", got, n)
			}
		})
	}
}

func TestRetryIdempotentOnly(t *testing.T) {
	var hits atomic.Int64
	busy := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "busy")
	}
	a, b := startBackend(t, busy), startBackend(t, busy)
	base := startProxyYAML(t, fmt.Sprintf(`upstreams:
  app: { backends: [{ url: %q }, { url: %q }] }
routes:
  - match: { path_prefix: / }
    upstream: app
    retry: { attempts: 1, on: [503] }
`, a.URL, b.URL))
	client := newClient(t)

	tests := []struct {
		method   string
		wantHits int64
	}{
		{http.MethodPost, 1},
		{http.MethodGet, 2},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			hits.Store(0)
			resp, got := send(t, client, tt.method, base+"/", "", strings.NewReader("x"))
			// Both ways, the client gets the last backend's 503 unchanged.
			if resp.StatusCode != http.StatusServiceUnavailable || got != "busy" {
				t.Errorf("got %d %q, want the backend's 503 \"busy\"", resp.StatusCode, got)
			}
			if n := hits.Load(); n != tt.wantHits {
				t.Errorf("backends saw %d requests, want %d", n, tt.wantHits)
			}
		})
	}
}

func TestIPHashSticky(t *testing.T) {
	const backends = 3
	var urls []string
	for i := range backends {
		b := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, i)
		})
		urls = append(urls, fmt.Sprintf("{ url: %q }", b.URL))
	}
	base := startProxyYAML(t, fmt.Sprintf(`trusted_proxies: ["127.0.0.1"]
upstreams:
  app: { balancer: ip_hash, backends: [%s] }
routes:
  - { match: { path_prefix: / }, upstream: app }
`, strings.Join(urls, ", ")))
	client := newClient(t)

	used := map[string]bool{}
	for c := range 30 {
		ip := fmt.Sprintf("203.0.113.%d", c+1)
		var first string
		for i := range 5 {
			resp, got := send(t, client, http.MethodGet, base+"/", ip, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("client %s request %d: status %d", ip, i, resp.StatusCode)
			}
			if i == 0 {
				first = got
			} else if got != first {
				t.Fatalf("client %s went to backend %s, then %s", ip, first, got)
			}
		}
		used[first] = true
	}
	// 30 clients all landing on one of 3 backends would mean the hash
	// ignores the client.
	if len(used) < 2 {
		t.Errorf("30 clients used backends %v, want more than one", used)
	}
}
