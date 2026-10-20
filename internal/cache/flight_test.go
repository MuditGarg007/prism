package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls until cond holds, failing the test after a few seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFlightOneLeader checks that 1,000 concurrent joins of a cold key give
// exactly one leader, and that every waiter gets its result.
func TestFlightOneLeader(t *testing.T) {
	f := NewFlight()
	const n = 1000
	var leaders, stored atomic.Int64
	var wg sync.WaitGroup
	release := make(chan struct{})
	for range n {
		wg.Go(func() {
			c, role := f.Join("k")
			switch role {
			case Lead:
				leaders.Add(1)
				<-release
				c.Done(Stored)
			case Wait:
				if c.Wait(context.Background(), time.Minute) == Stored {
					stored.Add(1)
				}
			default:
				t.Errorf("role %v, want Lead or Wait", role)
			}
		})
	}
	waitFor(t, func() bool { return f.Waiting() == n-1 })
	close(release)
	wg.Wait()
	if leaders.Load() != 1 || stored.Load() != n-1 {
		t.Errorf("%d leaders and %d waiters told Stored, want 1 and %d", leaders.Load(), stored.Load(), n-1)
	}
	if f.Waiting() != 0 || len(f.calls) != 0 {
		t.Errorf("after Done: %d waiting, %d calls; want 0 and 0", f.Waiting(), len(f.calls))
	}
	// The key is free again, so the next request leads.
	if _, role := f.Join("k"); role != Lead {
		t.Errorf("join after Done: role %v, want Lead", role)
	}
}

func TestFlightKeysAreIndependent(t *testing.T) {
	f := NewFlight()
	if _, role := f.Join("a"); role != Lead {
		t.Fatalf("a: role %v, want Lead", role)
	}
	if _, role := f.Join("b"); role != Lead {
		t.Errorf("b: role %v, want Lead", role)
	}
}

func TestFlightWaitTimesOut(t *testing.T) {
	f := NewFlight()
	lead, _ := f.Join("k")
	c, role := f.Join("k")
	if role != Wait {
		t.Fatalf("role %v, want Wait", role)
	}
	if got := c.Wait(context.Background(), 10*time.Millisecond); got != TimedOut {
		t.Errorf("Wait = %v, want TimedOut", got)
	}
	if got := c.Wait(context.Background(), 0); got != TimedOut {
		t.Errorf("Wait with no time left = %v, want TimedOut", got)
	}
	// A timed-out waiter does not disturb the leader.
	lead.Done(Stored)
	if got := c.Wait(context.Background(), 0); got != Stored {
		t.Errorf("Wait after Done = %v, want Stored", got)
	}
}

func TestFlightWaitCanceled(t *testing.T) {
	f := NewFlight()
	f.Join("k")
	c, _ := f.Join("k")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Result)
	go func() { got <- c.Wait(ctx, time.Minute) }()
	waitFor(t, func() bool { return f.Waiting() == 1 })
	cancel()
	if r := <-got; r != Canceled {
		t.Errorf("Wait = %v, want Canceled", r)
	}
	if f.Waiting() != 0 {
		t.Errorf("Waiting = %d after cancel, want 0", f.Waiting())
	}
}

// TestFlightFailedReleasesWaiters checks that a leader that stores nothing
// still wakes everyone, and leaves the key free rather than marked.
func TestFlightFailedReleasesWaiters(t *testing.T) {
	f := NewFlight()
	lead, _ := f.Join("k")
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range 10 {
		c, _ := f.Join("k")
		wg.Go(func() {
			if c.Wait(context.Background(), time.Minute) == Failed {
				failed.Add(1)
			}
		})
	}
	waitFor(t, func() bool { return f.Waiting() == 10 })
	lead.Done(Failed)
	wg.Wait()
	if failed.Load() != 10 {
		t.Errorf("%d waiters told Failed, want 10", failed.Load())
	}
	if _, role := f.Join("k"); role != Lead {
		t.Errorf("join after Failed: role %v, want Lead", role)
	}
}

// TestFlightDoneOnce checks that only the first Done counts, so a deferred
// Done(Failed) cannot undo the result reported before it.
func TestFlightDoneOnce(t *testing.T) {
	f := NewFlight()
	lead, _ := f.Join("k")
	c, _ := f.Join("k")
	lead.Done(Stored)
	lead.Done(Failed)
	if got := c.Wait(context.Background(), 0); got != Stored {
		t.Errorf("Wait = %v, want Stored", got)
	}
	next, _ := f.Join("k")
	lead.Done(Uncacheable) // must not touch next, the key's new leader
	if f.calls["k"] != next {
		t.Error("a late Done replaced the key's new leader")
	}
}

func TestFlightHitForPass(t *testing.T) {
	var clock atomic.Int64
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	f := newFlight(func() time.Time { return start.Add(time.Duration(clock.Load())) })

	lead, _ := f.Join("k")
	c, _ := f.Join("k")
	lead.Done(Uncacheable)
	if got := c.Wait(context.Background(), 0); got != Uncacheable {
		t.Errorf("Wait = %v, want Uncacheable", got)
	}
	if c, role := f.Join("k"); role != Pass || c != nil {
		t.Errorf("join while marked: %v, %v; want nil, Pass", c, role)
	}
	clock.Store(int64(PassFor - time.Nanosecond))
	if _, role := f.Join("k"); role != Pass {
		t.Errorf("join just before expiry: role %v, want Pass", role)
	}
	clock.Store(int64(PassFor))
	if _, role := f.Join("k"); role != Lead {
		t.Errorf("join at expiry: role %v, want Lead", role)
	}
}

// TestFlightSweep checks that expired markers for keys nobody asks for again
// are dropped, while live markers and running calls stay.
func TestFlightSweep(t *testing.T) {
	var clock atomic.Int64
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	f := newFlight(func() time.Time { return start.Add(time.Duration(clock.Load())) })

	old, _ := f.Join("old")
	old.Done(Uncacheable)
	clock.Store(int64(time.Second))
	young, _ := f.Join("young")
	young.Done(Uncacheable)
	f.Join("running")

	clock.Store(int64(PassFor + time.Millisecond)) // "old" expired, "young" not
	f.Join("other")
	if _, ok := f.calls["old"]; ok {
		t.Error("expired marker kept after a sweep")
	}
	for _, k := range []string{"young", "running", "other"} {
		if _, ok := f.calls[k]; !ok {
			t.Errorf("%q dropped by the sweep", k)
		}
	}
}

func BenchmarkFlightLead(b *testing.B) {
	f := NewFlight()
	b.ReportAllocs()
	for b.Loop() {
		c, _ := f.Join("GET http://example.com/a")
		c.Done(Stored)
	}
}
