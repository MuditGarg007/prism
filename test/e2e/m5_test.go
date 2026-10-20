package e2e

// The M5 tests: request IDs, the Prometheus metrics on the admin API, and
// reloading under load (the M5 exit criterion), through the real stack
// built from YAML.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/admin"
)

// TestRequestIDAndMetrics checks that each request gets its own ID and is
// counted on the admin API's /metrics, with the cache's counters beside it.
func TestRequestIDAndMetrics(t *testing.T) {
	var (
		mu  sync.Mutex
		ids []string // as the backend saw them
	)
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get("X-Request-Id"))
		mu.Unlock()
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, "hello")
	})
	front, adm := startCache(t, backend.URL, "{ enabled: true }", t.Output())

	var got []string
	for range 2 {
		resp, err := http.Get(front + "/x")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		got = append(got, resp.Header.Get("X-Request-Id"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 1 || got[0] != ids[0] || got[1] == "" || got[1] == got[0] {
		t.Errorf("backend saw IDs %q, clients got %q; want the miss's ID on both sides and a new one for the hit", ids, got)
	}

	// Requests are counted as the access log finishes them, just after the
	// response, so the count may lag for a moment.
	want := []string{
		`prism_requests_total{code="2xx",route="/",upstream="app"} 2`,
		`prism_upstream_response_seconds_count{backend="` + backend.URL + `",upstream="app"} 1`,
		`prism_cache_requests_total{result="HIT"} 1`,
		`prism_backend_healthy{backend="` + backend.URL + `",upstream="app"} 1`,
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(adm + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var missing []string
		for _, w := range want {
			if !strings.Contains(string(body), w+"\n") {
				missing = append(missing, w)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/metrics lacks %q", missing)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// countedBackend is a backend that counts the requests and connections it
// gets, and answers after a millisecond so that requests overlap reloads.
type countedBackend struct {
	*httptest.Server
	requests, conns atomic.Int64
}

func startCounted(t *testing.T) *countedBackend {
	t.Helper()
	b := &countedBackend{}
	b.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			b.requests.Add(1)
			time.Sleep(time.Millisecond)
		}
		io.WriteString(w, "ok")
	}))
	b.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			b.conns.Add(1)
		}
	}
	b.Start()
	t.Cleanup(b.Close)
	return b
}

// upstreams returns the admin API's GET /upstreams, keyed by upstream name.
func upstreams(t *testing.T, adm string) map[string]admin.UpstreamInfo {
	t.Helper()
	resp, err := http.Get(adm + "/upstreams")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body admin.Upstreams
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	out := map[string]admin.UpstreamInfo{}
	for _, u := range body.Upstreams {
		out[u.Name] = u
	}
	return out
}

// TestReloadUnderLoad is the M5 exit test: workers send requests without a
// pause while the config alternates between a weight change, an added
// backend and a changed response header, reloaded through POST /reload.
// No request may fail, the backends that stay must keep their pooled
// connections, least_conn's inflight counts must settle at zero, and a
// backend that health checks marked down and whose breaker is open must
// stay that way.
func TestReloadUnderLoad(t *testing.T) {
	b1, b2, b3 := startCounted(t), startCounted(t), startCounted(t)
	var failHealth atomic.Bool
	sick := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !failHealth.Load() {
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	backend := func(b *countedBackend, weight int) string {
		return fmt.Sprintf("      - { url: %q, weight: %d }\n", b.URL, weight)
	}
	variants := []struct{ backends, gen string }{
		{backend(b1, 1) + backend(b2, 1), "a"},
		{backend(b1, 3) + backend(b2, 1), "a"},
		{backend(b1, 1) + backend(b2, 1) + backend(b3, 1), "a"},
		{backend(b1, 1) + backend(b2, 1), "b"},
	}
	config := func(i int) string {
		v := variants[i%len(variants)]
		return fmt.Sprintf(`
admin: { listen: "localhost:0" }
upstreams:
  app:
    balancer: least_conn
    backends:
%s    health: { path: /healthz, interval: 50ms, timeout: 40ms, unhealthy_after: 2, healthy_after: 2 }
    circuit_breaker: { failure_ratio: 0.5, min_requests: 50, open_for: 1s, window: 1s }
    max_idle_conns_per_host: 64
  side:
    backends: [{ url: %q }]
    health: { path: /healthz, interval: 50ms, timeout: 40ms, unhealthy_after: 2, healthy_after: 2 }
    circuit_breaker: { failure_ratio: 0.5, min_requests: 3, open_for: 1h, window: 10s }
routes:
  - { match: { path_prefix: /side/ }, upstream: side }
  - match: { path_prefix: / }
    upstream: app
    headers: { response_set: { X-Gen: %q } }
`, v.backends, sick.URL, v.gen)
	}
	srv := launch(t, config(0), io.Discard)
	front, adm := "http://"+srv.Addrs()[0].String(), "http://"+srv.AdminAddr().String()

	// Open the sick backend's breaker, then fail its health checks.
	for range 3 {
		if code, _, err := get(http.DefaultClient, front+"/side/x"); code != http.StatusInternalServerError {
			t.Fatalf("side: %d %v", code, err)
		}
	}
	failHealth.Store(true)
	sickState := func() admin.BackendInfo { return upstreams(t, adm)["side"].Backends[0] }
	eventually(t, "the sick backend to be marked down", func() bool { return !sickState().Healthy })
	if s := sickState().Breaker; s != "open" {
		t.Fatalf("sick breaker %q, want open", s)
	}

	const workers = 8
	var (
		stop         atomic.Bool
		wg           sync.WaitGroup
		sent, failed atomic.Int64
		firstErr     atomic.Value
		sawGenB      atomic.Bool
	)
	for range workers {
		client := newClient(t)
		wg.Go(func() {
			for !stop.Load() {
				resp, err := client.Get(front + "/")
				sent.Add(1)
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if err == nil && resp.StatusCode != http.StatusOK {
						err = fmt.Errorf("status %d", resp.StatusCode)
					}
					if resp.Header.Get("X-Gen") == "b" {
						sawGenB.Store(true)
					}
				}
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				}
			}
		})
	}

	const reloads = 40
	for i := 1; i <= reloads; i++ {
		writeConfig(t, srv.path, config(i))
		resp, err := http.Post(adm+"/reload", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reload %d: %d %s", i, resp.StatusCode, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Right after the last reload, before a fresh health check could have
	// passed or failed twice.
	if s := sickState(); s.Healthy || s.Breaker != "open" {
		t.Errorf("sick backend after reloads: healthy %v, breaker %q; want down and open", s.Healthy, s.Breaker)
	}
	stop.Store(true)
	wg.Wait()

	if n := failed.Load(); n > 0 {
		t.Errorf("%d of %d requests failed, the first with %v", n, sent.Load(), firstErr.Load())
	}
	t.Logf("%d requests over %d reloads; connections: b1 %d, b2 %d, b3 %d",
		sent.Load(), reloads, b1.conns.Load(), b2.conns.Load(), b3.conns.Load())
	// Without carrying transports over, every reload would dial up to one
	// connection per worker again. b3 is left out: a reload that drops it
	// closes its connections.
	if n, limit := b1.conns.Load()+b2.conns.Load(), int64(2*(workers+2)); n > limit {
		t.Errorf("%d connections to b1 and b2, want at most %d", n, limit)
	}
	if b3.requests.Load() == 0 || !sawGenB.Load() {
		t.Errorf("reloaded configs not applied: b3 served %d, X-Gen b seen %v", b3.requests.Load(), sawGenB.Load())
	}
	eventually(t, "inflight to settle at zero", func() bool {
		for _, b := range upstreams(t, adm)["app"].Backends {
			if b.Inflight != 0 {
				return false
			}
		}
		return true
	})
}
