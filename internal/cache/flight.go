package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// PassFor is how long a key whose response could not be stored skips
// coalescing (Varnish's hit-for-pass). Without it, every request for an
// uncacheable URL would queue behind the one before it.
const PassFor = 10 * time.Second

// Role says what a request that joined a Flight must do.
type Role int

const (
	// Lead: fetch the response, store it if possible, then call Done.
	Lead Role = iota
	// Wait: another request is fetching; call Wait, then look again.
	Wait
	// Pass: the key recently proved uncacheable; fetch without coalescing.
	Pass
)

// Result is how a wait ended: with the leader's outcome, or without it.
type Result int

const (
	// Failed: the leader got no response it could share, such as after a
	// backend error. Waiters fetch for themselves.
	Failed Result = iota
	// Stored: the leader stored a response, so waiters look it up again.
	Stored
	// Uncacheable: the response may not be stored. The key is marked for
	// PassFor, and waiters fetch for themselves.
	Uncacheable
	// TimedOut: the waiter gave up before the leader finished.
	TimedOut
	// Canceled: the waiter's context ended first.
	Canceled
)

// Flight coalesces concurrent fetches of the same key, so that a cold key
// requested 1,000 times at once costs one backend request: the first caller
// leads and the rest wait for it. It is safe for concurrent use.
//
// Keys that turned out uncacheable are remembered in the same map for
// PassFor, so their requests neither wait nor lead.
type Flight struct {
	mu      sync.Mutex
	calls   map[string]*Call
	now     func() time.Time
	sweepAt time.Time    // when expired pass markers are next dropped
	waiting atomic.Int64 // callers blocked in Wait
}

// Call is one leader's fetch of a key, or, with a nil done, a pass marker.
type Call struct {
	f         *Flight
	key       string
	done      chan struct{} // closed by Done
	result    Result        // set before done is closed
	finished  bool          // guarded by f.mu
	passUntil time.Time
}

// NewFlight returns an empty Flight.
func NewFlight() *Flight { return newFlight(time.Now) }

func newFlight(now func() time.Time) *Flight {
	return &Flight{calls: make(map[string]*Call), now: now}
}

// Join registers a request for key and says what it must do. The Call is
// nil for Pass.
func (f *Flight) Join(key string) (*Call, Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if c, ok := f.calls[key]; ok {
		if c.done != nil {
			return c, Wait
		}
		if now.Before(c.passUntil) {
			return nil, Pass
		}
	}
	f.sweep(now)
	c := &Call{f: f, key: key, done: make(chan struct{})}
	f.calls[key] = c
	return c, Lead
}

// sweep drops expired pass markers, at most once per PassFor, so keys that
// are never asked for again do not stay in the map. f.mu must be held.
func (f *Flight) sweep(now time.Time) {
	if now.Before(f.sweepAt) {
		return
	}
	f.sweepAt = now.Add(PassFor)
	for k, c := range f.calls {
		if c.done == nil && !now.Before(c.passUntil) {
			delete(f.calls, k)
		}
	}
}

// Waiting returns how many requests are blocked waiting for a leader.
func (f *Flight) Waiting() int { return int(f.waiting.Load()) }

// Done ends the leader's fetch with res, which must be Failed, Stored or
// Uncacheable, and releases its waiters. Uncacheable marks the key for
// PassFor. Only the first call has any effect, so a leader can defer
// Done(Failed) as a fallback and still report its real result earlier.
func (c *Call) Done(res Result) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.finished {
		return
	}
	c.finished = true
	c.result = res
	// While c is unfinished, Join never replaces it, so it is still the
	// key's entry.
	if res == Uncacheable {
		f.calls[c.key] = &Call{passUntil: f.now().Add(PassFor)}
	} else {
		delete(f.calls, c.key)
	}
	close(c.done)
}

// Wait blocks until the leader is done, timeout passes or ctx ends, and
// returns the leader's result, TimedOut or Canceled.
func (c *Call) Wait(ctx context.Context, timeout time.Duration) Result {
	select {
	case <-c.done:
		return c.result
	default:
	}
	if timeout <= 0 {
		return TimedOut
	}
	c.f.waiting.Add(1)
	defer c.f.waiting.Add(-1)
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.done:
		return c.result
	case <-t.C:
		return TimedOut
	case <-ctx.Done():
		return Canceled
	}
}
