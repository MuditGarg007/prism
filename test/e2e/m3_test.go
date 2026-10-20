package e2e

// The M3 exit tests: a backend dying under load, health checks, the bounded
// queue, the retry budget and the ready path, each through the real stack
// built from YAML.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// logBuffer collects prism's log while also writing it to the test output.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	out io.Writer
}

func newLogBuffer(t *testing.T) *logBuffer { return &logBuffer{out: t.Output()} }

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	return l.out.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// get is a GET that is safe to call off the test goroutine: it reports
// failures instead of ending the test. The code is 0 when no response came.
func get(client *http.Client, url string) (int, string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestBackendDiesUnderLoad kills one of two backends while clients keep
// sending, and checks that no client sees an error, that the dead backend's
// breaker opens, and that afterwards all traffic goes to the live one.
// Health checks are left out: they would mark the backend down before the
// breaker's window fills with failures.
func TestBackendDiesUnderLoad(t *testing.T) {
	var liveHits atomic.Int64
	live := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		liveHits.Add(1)
		io.WriteString(w, "live")
	})
	doomed := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "doomed")
	})
	logs := newLogBuffer(t)
	url := startProxyLogged(t, fmt.Sprintf(`
upstreams:
  app:
    backends: [{ url: %q }, { url: %q }]
    circuit_breaker: { failure_ratio: 0.5, min_requests: 5, open_for: 1m, window: 1s }
routes:
  - match: { path_prefix: / }
    upstream: app
    retry: { attempts: 1, on: [connect_error] }
`, live.URL, doomed.URL), logs)

	client := newClient(t)
	var mu sync.Mutex
	var errs []string
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if code, body, err := get(client, url+"/"); code != 200 {
					mu.Lock()
					errs = append(errs, fmt.Sprintf("%d %s %v", code, body, err))
					mu.Unlock()
				}
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	doomed.Close()
	eventually(t, "the breaker to open", func() bool {
		return strings.Contains(logs.String(), "circuit breaker state changed")
	})
	close(stop)
	wg.Wait()
	if len(errs) > 0 {
		t.Errorf("%d client errors, first %q", len(errs), errs[0])
	}
	if out := logs.String(); !strings.Contains(out, "to=open") || !strings.Contains(out, "backend="+doomed.URL) {
		t.Errorf("breaker log does not name the dead backend opening")
	}

	before := liveHits.Load()
	for range 20 {
		if _, body := send(t, client, "GET", url+"/", "", nil); body != "live" {
			t.Fatalf("got %q after the backend died", body)
		}
	}
	if got := liveHits.Load() - before; got != 20 {
		t.Errorf("live backend got %d of 20 requests", got)
	}
}

// TestHealthCheckRemovesBackend checks that a backend whose health check
// fails stops getting traffic even though it still answers requests.
func TestHealthCheckRemovesBackend(t *testing.T) {
	good := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "good")
	})
	sick := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "sick")
	})
	logs := newLogBuffer(t)
	url := startProxyLogged(t, fmt.Sprintf(`
upstreams:
  app:
    backends: [{ url: %q }, { url: %q }]
    health: { path: /healthz, interval: 20ms, timeout: 10ms, unhealthy_after: 2 }
routes:
  - { match: { path_prefix: / }, upstream: app }
`, good.URL, sick.URL), logs)
	eventually(t, "the sick backend to be marked down", func() bool {
		return strings.Contains(logs.String(), "backend marked unhealthy")
	})
	client := newClient(t)
	for range 20 {
		if _, body := send(t, client, "GET", url+"/", "", nil); body != "good" {
			t.Fatalf("got %q from a backend marked down", body)
		}
	}
}

// TestQueue checks that a request over max_concurrency waits in the queue
// and is served once a slot frees, while one more than the queue holds is
// shed at once with 503.
func TestQueue(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int64
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-release
		io.WriteString(w, "ok")
	})
	url := startProxyYAML(t, fmt.Sprintf(`
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app, max_concurrency: 1, queue_size: 1, queue_timeout: 5s }
`, backend.URL))
	client := newClient(t)

	codes := make(chan int, 2)
	for range 2 {
		go func() {
			code, _, _ := get(client, url+"/")
			codes <- code
		}()
	}
	eventually(t, "one request at the backend", func() bool { return started.Load() == 1 })
	time.Sleep(50 * time.Millisecond) // let the second one reach the queue
	resp, _ := send(t, client, "GET", url+"/", "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("request over a full queue got %d Retry-After %q, want 503 and 1", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if started.Load() != 1 {
		t.Errorf("%d requests reached the backend while the slot was taken", started.Load())
	}
	close(release)
	for range 2 {
		if code := <-codes; code != 200 {
			t.Errorf("held or queued request got %d", code)
		}
	}
}

// TestRetryBudgetCapsLoad sends requests to three failing backends with two
// retries allowed per request. Without a budget the backends would see three
// attempts per request; the budget keeps it near 1 + budget_ratio.
func TestRetryBudgetCapsLoad(t *testing.T) {
	var hits atomic.Int64
	var urls []any
	for range 3 {
		b := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		urls = append(urls, b.URL)
	}
	logs := newLogBuffer(t)
	url := startProxyLogged(t, fmt.Sprintf(`
upstreams:
  app: { backends: [{ url: %q }, { url: %q }, { url: %q }] }
routes:
  - match: { path_prefix: / }
    upstream: app
    retry: { attempts: 2, on: ["503"], budget_ratio: 0.1, budget_min_per_sec: 1, backoff: { base: 1ms, max: 1ms } }
`, urls...), logs)
	client := newClient(t)
	const requests = 300
	for range requests {
		if resp, _ := send(t, client, "GET", url+"/", "", nil); resp.StatusCode != 503 {
			t.Fatalf("got %d, want the backends' 503", resp.StatusCode)
		}
	}
	// 0.1 × 300 requests, plus 1/s over the 10 s window.
	if got, limit := hits.Load(), int64(requests+30+10); got > limit || got < requests {
		t.Errorf("backends got %d attempts for %d requests, want at most %d", got, requests, limit)
	}
	if !strings.Contains(logs.String(), "retry_budget_exhausted=true") {
		t.Error("access log never says the budget ran out")
	}
}

func TestReadyPath(t *testing.T) {
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "routed")
	})
	url := startProxyYAML(t, fmt.Sprintf(`
ready_path: /readyz
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app }
`, backend.URL))
	client := newClient(t)
	if resp, body := send(t, client, "GET", url+"/readyz", "", nil); resp.StatusCode != 200 || body != "ready\n" {
		t.Errorf("ready path got %d %q", resp.StatusCode, body)
	}
	if _, body := send(t, client, "GET", url+"/readyzz", "", nil); body != "routed" {
		t.Errorf("other path got %q, want it routed", body)
	}
}
