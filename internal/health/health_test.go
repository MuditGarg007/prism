package health

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/balancer"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/upstream"
)

func TestTracker(t *testing.T) {
	tests := []struct {
		name    string
		results string // '+' passed, '-' failed
		flips   string // '^' where the state flips, '.' elsewhere
	}{
		{"healthy stays", "++++", "...."},
		{"down after three", "---", "..^"},
		{"a pass resets the streak", "--+--", "....."},
		{"up after two", "---++", "..^.^"},
		{"a fail resets the way back", "---+-++", "..^...^"},
		{"flapping never flips", "-+-+-+-+", "........"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := tracker{unhealthyAfter: 3, healthyAfter: 2}
			var got strings.Builder
			for _, r := range tt.results {
				if tr.observe(r == '+') {
					got.WriteByte('^')
				} else {
					got.WriteByte('.')
				}
			}
			if got.String() != tt.flips {
				t.Errorf("flips = %s, want %s", got.String(), tt.flips)
			}
		})
	}
}

func TestJitter(t *testing.T) {
	c := &Checker{cfg: Config{Interval: 10 * time.Second}}
	c.randN = func(n time.Duration) time.Duration { return 0 }
	if got := c.next(); got != 9*time.Second {
		t.Errorf("lowest next = %v, want 9s", got)
	}
	c.randN = func(n time.Duration) time.Duration { return n - 1 }
	if got := c.next(); got != 11*time.Second-1 {
		t.Errorf("highest next = %v, want just under 11s", got)
	}
	c.cfg.Interval = 4 // too short to spread
	if got := c.next(); got != 4 {
		t.Errorf("next = %v, want the interval as is", got)
	}
}

// flipBackend answers checks with 200 or 503, as set, and records the
// requests it got.
type flipBackend struct {
	*httptest.Server
	down atomic.Bool
	mu   sync.Mutex
	reqs []*http.Request
}

func newFlipBackend(t *testing.T) *flipBackend {
	fb := &flipBackend{}
	fb.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.reqs = append(fb.reqs, r)
		fb.mu.Unlock()
		if fb.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(fb.Close)
	return fb
}

func (fb *flipBackend) count() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return len(fb.reqs)
}

func newPool(t *testing.T, urls ...string) *upstream.Pool {
	t.Helper()
	u := &config.Upstream{MaxIdleConnsPerHost: 2}
	for _, url := range urls {
		u.Backends = append(u.Backends, config.Backend{URL: url, Weight: 1})
	}
	u.Timeouts.Connect = time.Second
	p, err := upstream.NewPool("web", u, balancer.New(config.BalancerRoundRobin), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// lockedBuffer is a log sink that the check goroutines and the test can use
// at once.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCheckerFlips(t *testing.T) {
	fb := newFlipBackend(t)
	pool := newPool(t, fb.URL)
	b := pool.Backends[0]
	var logs lockedBuffer
	c := New(pool, Config{
		Path:           "/healthz?deep=1",
		Interval:       2 * time.Millisecond,
		Timeout:        time.Second,
		UnhealthyAfter: 3,
		HealthyAfter:   2,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	c.Start()
	defer c.Stop()

	waitFor(t, "a first check", func() bool { return fb.count() > 0 })
	if !b.Available() {
		t.Fatal("backend unhealthy while checks pass")
	}
	fb.down.Store(true)
	waitFor(t, "the backend to go down", func() bool { return !b.Available() })
	fb.down.Store(false)
	waitFor(t, "the backend to come back", func() bool { return b.Available() })

	out := logs.String()
	for _, want := range []string{
		`msg="backend marked unhealthy" upstream=web backend=` + fb.URL + ` checks=3 error="status 503"`,
		`msg="backend marked healthy" upstream=web backend=` + fb.URL + ` checks=2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs %q do not contain %q", out, want)
		}
	}

	fb.mu.Lock()
	r := fb.reqs[0]
	fb.mu.Unlock()
	if r.Method != http.MethodGet || r.URL.RequestURI() != "/healthz?deep=1" ||
		r.Host != strings.TrimPrefix(fb.URL, "http://") || r.UserAgent() != "prism-health-check" {
		t.Errorf("check was %s %s Host=%s UA=%q", r.Method, r.URL.RequestURI(), r.Host, r.UserAgent())
	}
}

func TestCheckerUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	fb := newFlipBackend(t)
	pool := newPool(t, dead.URL, fb.URL)
	c := New(pool, Config{Path: "/", Interval: time.Millisecond, Timeout: time.Second, UnhealthyAfter: 1, HealthyAfter: 1}, slog.New(slog.DiscardHandler))
	c.Start()
	defer c.Stop()

	waitFor(t, "the dead backend to go down", func() bool { return !pool.Backends[0].Available() })
	waitFor(t, "checks of the live backend", func() bool { return fb.count() >= 5 })
	if !pool.Backends[1].Available() {
		t.Error("live backend marked unhealthy")
	}
}

func TestCheckerTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	pool := newPool(t, slow.URL)
	c := New(pool, Config{Path: "/", Interval: time.Millisecond, Timeout: 5 * time.Millisecond, UnhealthyAfter: 2, HealthyAfter: 1}, slog.New(slog.DiscardHandler))
	c.Start()
	defer c.Stop()
	waitFor(t, "the slow backend to go down", func() bool { return !pool.Backends[0].Available() })
}

// TestCheckerStop checks that Stop waits for the goroutines, so no check is
// sent after it returns, and that Stop without Start is fine.
func TestCheckerStop(t *testing.T) {
	New(newPool(t, "http://127.0.0.1:1"), Config{Interval: time.Second}, slog.New(slog.DiscardHandler)).Stop()

	fb := newFlipBackend(t)
	c := New(newPool(t, fb.URL), Config{Path: "/", Interval: time.Millisecond, Timeout: time.Second, UnhealthyAfter: 1, HealthyAfter: 1}, slog.New(slog.DiscardHandler))
	c.Start()
	waitFor(t, "a few checks", func() bool { return fb.count() >= 3 })
	c.Stop()
	c.Stop()
	// A check sent just before Stop may still be on its way in.
	time.Sleep(5 * time.Millisecond)
	n := fb.count()
	time.Sleep(20 * time.Millisecond)
	if got := fb.count(); got != n {
		t.Errorf("%d checks after Stop", got-n)
	}
}

// TestCheckerStartsFromState checks that a checker started on a backend
// that is already down, as after a reload, needs healthy_after passing
// checks to bring it back rather than trusting it at once.
func TestCheckerStartsFromState(t *testing.T) {
	fb := newFlipBackend(t)
	pool := newPool(t, fb.URL)
	b := pool.Backends[0]
	b.SetHealthy(false)
	c := New(pool, Config{Path: "/", Interval: time.Millisecond, Timeout: time.Second, UnhealthyAfter: 1, HealthyAfter: 3}, slog.New(slog.DiscardHandler))
	c.Start()
	defer c.Stop()
	waitFor(t, "the backend to come back", b.Healthy)
	if n := fb.count(); n < 3 {
		t.Errorf("healthy again after %d checks, want at least 3", n)
	}
}
