package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/admin"
)

// These are the M8 exit tests: the hardening of auth, the admin API, the
// cache, health checks across reloads, and ip_hash with a canary.

// TestCanaryIPHash runs a 20% canary with ip_hash on both upstreams. The
// split and the rings hash the same client addresses; each side's clients
// must still spread evenly over that side's backends, and every client must
// stay where it first landed. (Each ring has thousands of points per
// backend, so any slice of the hash space holds an even share of every
// backend's points: even a split taken from the top bits of the ring's own
// hash, without the salt, passes this test.)
func TestCanaryIPHash(t *testing.T) {
	const perSide = 4
	backends := func(side string) string {
		var urls []string
		for i := range perSide {
			b := startBackend(t, named(fmt.Sprintf("%s-%d", side, i)))
			urls = append(urls, fmt.Sprintf("{ url: %q }", b.URL))
		}
		return strings.Join(urls, ", ")
	}
	stable, canary := backends("stable"), backends("canary")
	front := startProxyYAML(t, fmt.Sprintf(`
trusted_proxies: [127.0.0.1]
upstreams:
  stable: { balancer: ip_hash, backends: [%s] }
  canary: { balancer: ip_hash, backends: [%s] }
routes:
  - { match: { path_prefix: / }, upstream: stable, canary: { upstream: canary, percent: 20 } }
`, stable, canary))
	client := newClient(t)
	backend := func(ip string) string {
		req, _ := http.NewRequest("GET", front+"/", nil)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	ip := func(i int) string { return fmt.Sprintf("10.%d.%d.%d", i/65536, i/256%256, i%256) }

	const clients = 4000
	first := make([]string, clients)
	counts := map[string]int{}
	sides := map[string]int{}
	for i := range clients {
		first[i] = backend(ip(i))
		side, _, _ := strings.Cut(first[i], "-")
		counts[first[i]]++
		sides[side]++
	}
	if share := float64(sides["canary"]) / clients; share < 0.17 || share > 0.23 {
		t.Errorf("canary share %.3f, want 0.20 ± 0.03", share)
	}
	// About 200 canary clients per backend, so sampling alone moves a
	// share by several percent.
	for _, side := range []string{"stable", "canary"} {
		for i := range perSide {
			name := fmt.Sprintf("%s-%d", side, i)
			share := float64(counts[name]) / float64(sides[side])
			if share < 0.75/perSide || share > 1.25/perSide {
				t.Errorf("%s got %.3f of the %s side's clients, want %.3f ± 25%%", name, share, side, 1.0/perSide)
			}
		}
	}
	for i := 0; i < clients; i += 20 {
		if got := backend(ip(i)); got != first[i] {
			t.Errorf("client %s moved from %s to %s", ip(i), first[i], got)
		}
	}
}

// cpuTime is the CPU this process has used, user and system.
func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestAuthFailureBurst sends wrong passwords until the failed-auth limit is
// reached, then a burst of 100 more. The burst gets 429 with Retry-After,
// and costs less CPU than the 5 bcrypt runs before it: past the limit, no
// guess reaches bcrypt. The right password is refused too until the limit
// refills, while another client is unaffected.
func TestAuthFailureBurst(t *testing.T) {
	backend := startBackend(t, named("ok"))
	front := startProxyYAML(t, fmt.Sprintf(`
trusted_proxies: [127.0.0.1]
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - match: { path_prefix: / }
    upstream: app
    auth: { basic: { realm: api, users: { alice: %q } }, failure_limit: 5/m }
`, backend.URL, aliceHash))
	client := newClient(t)
	try := func(ip, pass string) (*http.Response, error) {
		req, _ := http.NewRequest("GET", front+"/", nil)
		req.Header.Set("X-Forwarded-For", ip)
		req.SetBasicAuth("alice", pass)
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return resp, err
	}

	start := cpuTime(t)
	for i := range 5 {
		if resp, err := try("10.0.0.1", "wrong"); err != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("guess %d: %v %v, want 401", i, resp, err)
		}
	}
	bcrypts := cpuTime(t) - start

	const burst, workers = 100, 10
	var wg sync.WaitGroup
	var refused atomic.Int64
	start = cpuTime(t)
	for range workers {
		wg.Go(func() {
			for range burst / workers {
				resp, err := try("10.0.0.1", "wrong")
				if err == nil && resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "" {
					refused.Add(1)
				}
			}
		})
	}
	wg.Wait()
	flood := cpuTime(t) - start
	if n := refused.Load(); n != burst {
		t.Errorf("%d of %d guesses past the limit got 429 with Retry-After", n, burst)
	}
	if flood >= bcrypts {
		t.Errorf("%d refused guesses cost %v of CPU, more than the %v of 5 bcrypt checks", burst, flood, bcrypts)
	}
	t.Logf("5 checked guesses: %v of CPU; %d refused: %v", bcrypts, burst, flood)

	if resp, err := try("10.0.0.1", "s3cret"); err != nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("right password over the limit: %v %v, want 429", resp, err)
	}
	if resp, err := try("10.0.0.2", "s3cret"); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("another client: %v %v, want 200", resp, err)
	}
}

// adminYAML is a proxy to backend with its admin API on loopback behind
// a bearer key, key-one.
func adminYAML(backend string) string {
	return fmt.Sprintf(`
admin:
  listen: "localhost:0"
  auth: { api_keys: { header: Authorization, keys: { prometheus: %q } } }
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app }
`, keyDigest, backend)
}

// TestAdminAuth checks that every admin endpoint but the probes needs
// credentials, and that a refused POST /reload does not reload.
func TestAdminAuth(t *testing.T) {
	backend := startBackend(t, named("ok"))
	srv := launch(t, adminYAML(backend.URL), io.Discard)
	adm := "http://" + srv.AdminAddr().String()
	client := newClient(t)
	call := func(method, path, authz string) int {
		req, _ := http.NewRequest(method, adm+path, nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	// A config that would fail to load, so a reload that ran would fail.
	writeConfig(t, srv.path, "upstreams: {}\n")
	for _, tt := range []struct {
		method, path, authz string
		want                int
	}{
		{"GET", "/routes", "", http.StatusUnauthorized},
		{"GET", "/config", "Bearer key-two", http.StatusUnauthorized},
		{"GET", "/metrics", "", http.StatusUnauthorized},
		{"POST", "/cache/purge", "", http.StatusUnauthorized},
		{"POST", "/reload", "", http.StatusUnauthorized},
		{"GET", "/ready", "", http.StatusOK},
		{"GET", "/live", "", http.StatusOK},
		{"GET", "/routes", "Bearer key-one", http.StatusOK},
		{"GET", "/metrics", "Bearer key-one", http.StatusOK},
	} {
		if got := call(tt.method, tt.path, tt.authz); got != tt.want {
			t.Errorf("%s %s with %q: %d, want %d", tt.method, tt.path, tt.authz, got, tt.want)
		}
	}
	if got := call("POST", "/reload", "Bearer key-one"); got == http.StatusOK {
		t.Error("the broken config reloaded, so the earlier POST /reload may have run too")
	}
}

// TestAdminReadyAtShutdown checks the admin port's probes across the start
// of a graceful shutdown, which main begins with Drain: /ready turns 503
// while /live and the proxy keep answering.
func TestAdminReadyAtShutdown(t *testing.T) {
	backend := startBackend(t, named("ok"))
	srv := launch(t, adminYAML(backend.URL), io.Discard)
	front, adm := "http://"+srv.Addrs()[0].String(), "http://"+srv.AdminAddr().String()
	client := newClient(t)
	status := func(url string) int {
		code, _, err := get(client, url)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	if got := status(adm + "/ready"); got != http.StatusOK {
		t.Fatalf("/ready before shutdown: %d, want 200", got)
	}
	srv.Drain()
	for _, tt := range []struct {
		url  string
		want int
	}{
		{adm + "/ready", http.StatusServiceUnavailable},
		{adm + "/live", http.StatusOK},
		{front + "/", http.StatusOK},
	} {
		if got := status(tt.url); got != tt.want {
			t.Errorf("%s while draining: %d, want %d", tt.url, got, tt.want)
		}
	}
}

// TestCacheStaleWhileRevalidate lets an entry go stale, then sends 50
// concurrent requests while the backend takes 600 ms to answer. Every
// request gets the stale entry at once as UPDATING, exactly one background
// request reaches the backend, and its answer is served from then on. The
// response delay counts toward a response's age (RFC 9111, section 4.2.3),
// so the new entry arrives with 600 ms of its 2 s max-age gone.
func TestCacheStaleWhileRevalidate(t *testing.T) {
	var (
		requests atomic.Int64
		slow     atomic.Bool
	)
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if slow.Load() {
			time.Sleep(600 * time.Millisecond)
		}
		w.Header().Set("Cache-Control", "max-age=2")
		fmt.Fprintf(w, "v%d", n)
	})
	front, adm := startCache(t, backend.URL, "{ enabled: true, stale_while_revalidate: 10s }", io.Discard)
	client := newClient(t)
	if code, xcache, body, err := fetch(client, front+"/page", nil); err != nil || code != 200 || xcache != "MISS" || body != "v1" {
		t.Fatalf("first request: %d %s %q %v", code, xcache, body, err)
	}
	time.Sleep(2200 * time.Millisecond) // past max-age, within the window
	slow.Store(true)

	const clients = 50
	var wg sync.WaitGroup
	errs := make(chan string, clients)
	for range clients {
		wg.Go(func() {
			start := time.Now()
			code, xcache, body, err := fetch(client, front+"/page", nil)
			if took := time.Since(start); err != nil || code != 200 || xcache != "UPDATING" || body != "v1" || took > 300*time.Millisecond {
				errs <- fmt.Sprintf("%d %s %q %v after %v", code, xcache, body, err, took)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("stale request: got %s, want UPDATING v1 at once", e)
	}
	if st := cacheStats(t, client, adm); st.Updating != clients {
		t.Errorf("stats count %d UPDATING, want %d", st.Updating, clients)
	}

	eventually(t, "the background revalidation", func() bool {
		_, xcache, body, _ := fetch(client, front+"/page", nil)
		return xcache == "HIT" && body == "v2"
	})
	if n := requests.Load(); n != 2 {
		t.Errorf("backend saw %d requests, want 2: the first and one revalidation", n)
	}
}

// TestCacheLeaderDrainTimeout has a backend send half a cacheable body
// and stall. The client leaves then; the leader, which keeps reading for
// the cache, gives up lock_timeout later, cancelling the backend request,
// and stores nothing.
func TestCacheLeaderDrainTimeout(t *testing.T) {
	const lockTimeout = 300 * time.Millisecond
	var first atomic.Bool
	first.Store(true)
	stalled, canceled := make(chan struct{}), make(chan time.Time, 1)
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Length", "10")
		if !first.CompareAndSwap(true, false) {
			io.WriteString(w, "0123456789")
			return
		}
		io.WriteString(w, "01234")
		w.(http.Flusher).Flush()
		close(stalled)
		select {
		case <-r.Context().Done():
			canceled <- time.Now()
		case <-time.After(10 * time.Second):
		}
	})
	front, _ := startCache(t, backend.URL, fmt.Sprintf("{ enabled: true, lock_timeout: %s }", lockTimeout), io.Discard)
	client := newClient(t)

	// The proxy buffers a body this small, so the client sees nothing
	// before it leaves.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", front+"/page", nil)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		if resp, err := client.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-stalled
	left := time.Now()
	cancel()
	<-gone

	select {
	case at := <-canceled:
		if d := at.Sub(left); d < lockTimeout || d > lockTimeout+2*time.Second {
			t.Errorf("the backend request was canceled %v after the client left, want lock_timeout (%v) after", d, lockTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader kept reading the stalled body after its client left")
	}
	// Nothing was stored, so the next request fetches the whole body.
	if code, xcache, body, err := fetch(client, front+"/page", nil); err != nil || code != 200 || xcache != "MISS" || body != "0123456789" {
		t.Errorf("after the abandoned fill: %d %s %q %v, want a MISS", code, xcache, body, err)
	}
}

// TestHealthReloadStorm reloads every 0.25 s, faster than the 0.3 s
// health check interval, while a backend fails every check. Each reload
// replaces the checker, which carries on the streak and the schedule, so
// the backend is still marked down after unhealthy_after checks. Before
// M8 every new checker started its streak again at a random offset, and
// never reached 3.
func TestHealthReloadStorm(t *testing.T) {
	var checks atomic.Int64
	sick := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			checks.Add(1)
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	config := func(gen int) string {
		return fmt.Sprintf(`
admin: { listen: "localhost:0" }
upstreams:
  app:
    backends: [{ url: %q }]
    health: { path: /healthz, interval: 300ms, timeout: 200ms, unhealthy_after: 3 }
routes:
  - { match: { path_prefix: / }, upstream: app, headers: { response_set: { X-Gen: "%d" } } }
`, sick.URL, gen)
	}
	srv := launch(t, config(0), io.Discard)
	adm := "http://" + srv.AdminAddr().String()

	// Clients of their own, closed before the proxy shuts down: two
	// goroutines sharing http.DefaultClient can leave it a dialed but
	// unused connection, which Shutdown waits 5 s for.
	reloads, polls := newClient(t), newClient(t)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for gen := 1; ; gen++ {
			select {
			case <-stop:
				done <- nil
				return
			case <-tick.C:
			}
			writeConfig(t, srv.path, config(gen))
			resp, err := reloads.Post(adm+"/reload", "", nil)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					err = fmt.Errorf("reload %d: status %d", gen, resp.StatusCode)
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	start := time.Now()
	eventually(t, "the failing backend to be marked down", func() bool {
		_, body, err := get(polls, adm+"/upstreams")
		var ups admin.Upstreams
		if err != nil || json.Unmarshal([]byte(body), &ups) != nil {
			return false
		}
		return !ups.Upstreams[0].Backends[0].Healthy
	})
	took := time.Since(start)
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := checks.Load(); n < 3 {
		t.Errorf("marked down after %d checks, want at least unhealthy_after (3)", n)
	}
	t.Logf("marked down after %v and %d checks", took, checks.Load())
}
