package resilience

import (
	"bytes"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a clock that starts at t0 and moves only when told to.
type fakeClock struct{ d atomic.Int64 }

func (c *fakeClock) now() time.Time          { return t0.Add(time.Duration(c.d.Load())) }
func (c *fakeClock) advance(d time.Duration) { c.d.Add(int64(d)) }

var testBreakerConfig = BreakerConfig{
	FailureRatio:   0.5,
	MinRequests:    4,
	OpenFor:        30 * time.Second,
	Window:         10 * time.Second,
	HalfOpenProbes: 2,
}

// newTestBreaker returns a breaker on a fake clock that records its state
// changes.
func newTestBreaker(t *testing.T, cfg BreakerConfig) (*Breaker, *fakeClock, func() []string) {
	t.Helper()
	clk := &fakeClock{}
	var mu sync.Mutex
	var changes []string
	b := NewBreaker(cfg, func(from, to BreakerState) {
		mu.Lock()
		changes = append(changes, from.String()+">"+to.String())
		mu.Unlock()
	})
	b.origin, b.now, b.window = t0, clk.now, NewWindow(cfg.Window, t0)
	return b, clk, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(changes)
	}
}

// do sends one request through b and reports out for it.
func do(t *testing.T, b *Breaker, out Outcome) {
	t.Helper()
	tk, ok := b.Allow()
	if !ok {
		t.Fatalf("Allow refused in state %v", b.State())
	}
	b.Report(tk, out)
}

func TestBreakerOpensAtRatio(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []Outcome
		want     BreakerState
	}{
		{"below min_requests", []Outcome{Failure, Failure, Failure}, BreakerClosed},
		{"ratio reached at min_requests", []Outcome{Success, Failure, Success, Failure}, BreakerOpen},
		{"ratio not reached", []Outcome{Success, Success, Success, Failure, Success, Failure}, BreakerClosed},
		{"neutral does not count", []Outcome{Failure, Neutral, Neutral, Failure, Failure}, BreakerClosed},
		{"only failures check", []Outcome{Failure, Failure, Failure, Success}, BreakerClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _, _ := newTestBreaker(t, testBreakerConfig)
			for _, out := range tt.outcomes {
				do(t, b, out)
			}
			if got := b.State(); got != tt.want {
				t.Errorf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBreakerWindowSlides(t *testing.T) {
	b, clk, _ := newTestBreaker(t, testBreakerConfig)
	for range 3 {
		do(t, b, Failure)
	}
	clk.advance(11 * time.Second)
	do(t, b, Failure)
	if b.State() != BreakerClosed {
		t.Fatal("failures from an earlier window opened the breaker")
	}
	for range 3 {
		do(t, b, Failure)
	}
	if b.State() != BreakerOpen {
		t.Fatal("4 failures in one window did not open the breaker")
	}
}

func TestBreakerRecovers(t *testing.T) {
	b, clk, changes := newTestBreaker(t, testBreakerConfig)
	for range 4 {
		do(t, b, Failure)
	}
	if _, ok := b.Allow(); ok || b.Ready() {
		t.Fatal("open breaker admitted a request")
	}
	clk.advance(30*time.Second - 1)
	if _, ok := b.Allow(); ok || b.Ready() {
		t.Fatal("breaker admitted a request before open_for passed")
	}

	clk.advance(1)
	if !b.Ready() {
		t.Fatal("not ready once open_for passed")
	}
	p1, ok1 := b.Allow()
	p2, ok2 := b.Allow()
	if !ok1 || !ok2 || b.State() != BreakerHalfOpen {
		t.Fatalf("half-open admitted %v, %v in state %v; want both probes", ok1, ok2, b.State())
	}
	if _, ok := b.Allow(); ok || b.Ready() {
		t.Fatal("half-open admitted more than half_open_probes")
	}
	b.Report(p1, Success)
	if b.State() != BreakerHalfOpen {
		t.Fatal("closed before every probe succeeded")
	}
	b.Report(p2, Success)
	if b.State() != BreakerClosed {
		t.Fatalf("state = %v after every probe succeeded, want closed", b.State())
	}
	// The window was reset, so the failures that opened it are gone.
	do(t, b, Failure)
	if b.State() != BreakerClosed {
		t.Fatal("failures from before the breaker closed still count")
	}
	want := []string{"closed>open", "open>half_open", "half_open>closed"}
	if got := changes(); !slices.Equal(got, want) {
		t.Errorf("changes = %q, want %q", got, want)
	}
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	b, clk, changes := newTestBreaker(t, testBreakerConfig)
	for range 4 {
		do(t, b, Failure)
	}
	clk.advance(30 * time.Second)
	p1, _ := b.Allow()
	p2, _ := b.Allow()
	b.Report(p1, Failure)
	if b.State() != BreakerOpen {
		t.Fatalf("state = %v after a failed probe, want open", b.State())
	}
	// The other probe's success belongs to the half-open period that ended.
	b.Report(p2, Success)
	if _, ok := b.Allow(); ok {
		t.Fatal("reopened breaker admitted a request")
	}
	clk.advance(30 * time.Second)
	if _, ok := b.Allow(); !ok {
		t.Fatal("open_for restarted from the failed probe but did not end")
	}
	want := []string{"closed>open", "open>half_open", "half_open>open", "open>half_open"}
	if got := changes(); !slices.Equal(got, want) {
		t.Errorf("changes = %q, want %q", got, want)
	}
}

func TestBreakerNeutralProbeFreesSlot(t *testing.T) {
	b, clk, _ := newTestBreaker(t, testBreakerConfig)
	for range 4 {
		do(t, b, Failure)
	}
	clk.advance(30 * time.Second)
	p1, _ := b.Allow()
	p2, _ := b.Allow()
	b.Report(p1, Neutral)
	p3, ok := b.Allow()
	if !ok {
		t.Fatal("a neutral probe did not free its slot")
	}
	b.Report(p2, Success)
	b.Report(p3, Success)
	if b.State() != BreakerClosed {
		t.Errorf("state = %v, want closed after two successful probes", b.State())
	}
}

func TestBreakerIgnoresStaleTickets(t *testing.T) {
	b, clk, _ := newTestBreaker(t, testBreakerConfig)
	slow, _ := b.Allow() // admitted while closed, reported much later
	for range 4 {
		do(t, b, Failure)
	}
	clk.advance(30 * time.Second)
	probe, _ := b.Allow()
	b.Report(slow, Failure)
	if b.State() != BreakerHalfOpen {
		t.Fatalf("a request from before the breaker opened failed a probe: state = %v", b.State())
	}
	b.Report(probe, Success)
	b.Report(slow, Success)
	if b.State() != BreakerHalfOpen {
		t.Fatal("a request from before the breaker opened counted as a probe")
	}
}

func TestBreakerOverturn(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		b, _, _ := newTestBreaker(t, testBreakerConfig)
		// Two successes and two failures would open it; here one of the
		// failures is a success overturned.
		do(t, b, Success)
		do(t, b, Success)
		do(t, b, Failure)
		tk, _ := b.Allow()
		b.Report(tk, Success)
		if s := b.State(); s != BreakerClosed {
			t.Fatalf("state %v at 1 failure in 4, want closed", s)
		}
		b.Overturn(tk)
		if s := b.State(); s != BreakerOpen {
			t.Errorf("state %v after overturning a success to make 2 failures in 4, want open", s)
		}
	})
	t.Run("half-open probe", func(t *testing.T) {
		b, clk, _ := newTestBreaker(t, testBreakerConfig)
		for range 4 {
			do(t, b, Failure)
		}
		clk.advance(testBreakerConfig.OpenFor)
		tk, _ := b.Allow()
		b.Report(tk, Success)
		b.Overturn(tk)
		if s := b.State(); s != BreakerOpen {
			t.Errorf("state %v after a probe was overturned, want open", s)
		}
	})
	t.Run("stale ticket", func(t *testing.T) {
		b, clk, _ := newTestBreaker(t, testBreakerConfig)
		old, _ := b.Allow()
		b.Report(old, Success)
		for range 3 {
			do(t, b, Failure)
		}
		clk.advance(testBreakerConfig.OpenFor)
		b.Allow() // half-open now
		b.Overturn(old)
		if s := b.State(); s != BreakerHalfOpen {
			t.Errorf("state %v after overturning a ticket from the closed state, want half_open", s)
		}
	})
}

func TestBreakerConcurrent(t *testing.T) {
	cfg := testBreakerConfig
	cfg.OpenFor = time.Millisecond
	cfg.HalfOpenProbes = 3
	b := NewBreaker(cfg, nil)
	var wg sync.WaitGroup
	var over atomic.Int32
	for g := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(g), 1))
			for range 5000 {
				tk, ok := b.Allow()
				if int(b.probes.Load()) > cfg.HalfOpenProbes {
					over.Add(1)
				}
				if ok {
					b.Report(tk, Outcome(r.IntN(3)))
				}
			}
		})
	}
	wg.Wait()
	if n := over.Load(); n > 0 {
		t.Errorf("more than half_open_probes probes admitted %d times", n)
	}
}

func BenchmarkBreakerSuccess(b *testing.B) {
	br := NewBreaker(testBreakerConfig, nil)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if br.Ready() {
				tk, _ := br.Allow()
				br.Report(tk, Success)
			}
		}
	})
}

// TestBreakerStateJSON checks that JSON logs name the state rather than
// printing its number.
func TestBreakerStateJSON(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "to", BreakerHalfOpen)
	if !strings.Contains(buf.String(), `"to":"half_open"`) {
		t.Errorf("logged %s", buf.String())
	}
}
