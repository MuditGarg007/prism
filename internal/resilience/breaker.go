package resilience

import (
	"sync"
	"sync/atomic"
	"time"
)

// Outcome is what one attempt on a backend says about that backend's health.
type Outcome uint8

const (
	// Neutral says nothing about the backend: the client went away, or its
	// request body failed.
	Neutral Outcome = iota
	Success
	// Failure is a connect error, a timeout or a 5xx response.
	Failure
)

// BreakerState is the state of a circuit breaker.
type BreakerState int32

const (
	BreakerClosed   BreakerState = iota // requests flow, outcomes are counted
	BreakerOpen                         // requests are refused until open_for passes
	BreakerHalfOpen                     // a few probe requests test the backend
)

func (s BreakerState) String() string {
	switch s {
	case BreakerClosed:
		return "closed"
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half_open"
	}
	return "unknown"
}

// MarshalText makes logs show the state's name, including JSON logs, which
// do not use String.
func (s BreakerState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// BreakerConfig sets when a Breaker opens and how it recovers.
type BreakerConfig struct {
	FailureRatio   float64       // open at this share of failures in the window
	MinRequests    int           // but only once the window holds this many
	OpenFor        time.Duration // how long to refuse before probing
	Window         time.Duration
	HalfOpenProbes int // probes let through at once, all of which must succeed
}

// Ticket ties an admitted request to the breaker state it was admitted in,
// so an outcome that arrives after the state has moved on is ignored. A slow
// request admitted while closed must not count as a half-open probe.
type Ticket struct{ gen uint64 }

// Breaker is a circuit breaker for one backend.
//
// Closed, it counts outcomes over a sliding window and opens once at least
// MinRequests of them have failed at FailureRatio or more. Open, it refuses
// everything for OpenFor. Then the next Allow moves it to half-open, where
// it admits up to HalfOpenProbes requests: it closes once all of them have
// succeeded and opens again on the first failure. A neutral outcome frees
// its probe slot for another request.
//
// Ready, and Allow while closed, are lock-free so that the common path takes
// the mutex only once, in Report.
type Breaker struct {
	cfg    BreakerConfig
	origin time.Time
	now    func() time.Time
	// onChange is called after every state change, outside the lock.
	onChange func(from, to BreakerState)

	// Written under mu, read without it by Ready and Allow.
	gen       atomic.Uint64 // bumped on every state change
	state     atomic.Int32
	openUntil atomic.Int64 // nanoseconds since origin
	probes    atomic.Int32 // half-open probes admitted and not yet reported neutral

	mu       sync.Mutex
	window   *Window
	probesOK int
}

// NewBreaker returns a closed breaker. onChange may be nil.
func NewBreaker(cfg BreakerConfig, onChange func(from, to BreakerState)) *Breaker {
	now := time.Now()
	return &Breaker{
		cfg:      cfg,
		origin:   now,
		now:      time.Now,
		onChange: onChange,
		window:   NewWindow(cfg.Window, now),
	}
}

// State returns the breaker's current state. An open breaker whose open_for
// has passed still reports open until the next Allow moves it on.
func (b *Breaker) State() BreakerState { return BreakerState(b.state.Load()) }

// Ready reports whether Allow might admit a request now. It is a hint for
// balancers: Allow can still refuse when probes race for the last slot.
func (b *Breaker) Ready() bool {
	switch BreakerState(b.state.Load()) {
	case BreakerOpen:
		return b.since(b.now()) >= b.openUntil.Load()
	case BreakerHalfOpen:
		return int(b.probes.Load()) < b.cfg.HalfOpenProbes
	}
	return true
}

// Allow reports whether a request may go to the backend. Every admitted
// request must be reported, with its ticket, exactly once.
func (b *Breaker) Allow() (Ticket, bool) {
	// gen is read before state, and set writes them the other way round, so
	// a closed state seen here can only pair with an older gen. That ticket
	// is then stale and its outcome is dropped, which costs one sample.
	if g := b.gen.Load(); b.State() == BreakerClosed {
		return Ticket{g}, true
	}
	b.mu.Lock()
	from := b.State()
	ok := b.admit()
	t, to := Ticket{b.gen.Load()}, b.State()
	b.mu.Unlock()
	b.changed(from, to)
	return t, ok
}

func (b *Breaker) admit() bool {
	switch b.State() {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if b.since(b.now()) < b.openUntil.Load() {
			return false
		}
		b.set(BreakerHalfOpen)
	}
	if int(b.probes.Load()) >= b.cfg.HalfOpenProbes {
		return false
	}
	b.probes.Add(1)
	return true
}

// Report records the outcome of a request that Allow admitted.
func (b *Breaker) Report(t Ticket, out Outcome) {
	b.mu.Lock()
	from := b.State()
	if t.gen == b.gen.Load() {
		b.record(out)
	}
	to := b.State()
	b.mu.Unlock()
	b.changed(from, to)
}

// Overturn turns a success already reported with t into a failure: the
// response's headers looked fine, but its body then failed. While closed,
// the request already counts in the window, so only its failure is added.
// While half-open, it fails the probe and opens the breaker again.
func (b *Breaker) Overturn(t Ticket) {
	b.mu.Lock()
	from := b.State()
	if t.gen == b.gen.Load() {
		switch from {
		case BreakerClosed:
			now := b.now()
			b.window.Add(now, Counts{Marked: 1})
			b.checkTrip(now)
		case BreakerHalfOpen:
			b.open(b.now())
		}
	}
	to := b.State()
	b.mu.Unlock()
	b.changed(from, to)
}

func (b *Breaker) record(out Outcome) {
	switch b.State() {
	case BreakerClosed:
		if out == Neutral {
			return
		}
		now := b.now()
		c := Counts{Total: 1}
		if out == Failure {
			c.Marked = 1
		}
		b.window.Add(now, c)
		if out == Failure {
			b.checkTrip(now)
		}
	case BreakerHalfOpen:
		switch out {
		case Neutral:
			b.probes.Add(-1)
		case Failure:
			b.open(b.now())
		case Success:
			if b.probesOK++; b.probesOK >= b.cfg.HalfOpenProbes {
				b.window.Reset()
				b.set(BreakerClosed)
			}
		}
	}
}

// checkTrip opens the breaker if the window has enough requests and enough
// of them failed. Only a failure can push the ratio up, so only a failure
// needs to check.
func (b *Breaker) checkTrip(now time.Time) {
	s := b.window.Sum(now)
	if s.Total >= int64(b.cfg.MinRequests) && float64(s.Marked) >= b.cfg.FailureRatio*float64(s.Total) {
		b.open(now)
	}
}

func (b *Breaker) open(now time.Time) {
	b.openUntil.Store(b.since(now) + int64(b.cfg.OpenFor))
	b.set(BreakerOpen)
}

// set moves to state s, starting a new generation so outstanding tickets
// stop counting.
func (b *Breaker) set(s BreakerState) {
	b.gen.Add(1)
	b.probes.Store(0)
	b.probesOK = 0
	b.state.Store(int32(s))
}

func (b *Breaker) changed(from, to BreakerState) {
	if from != to && b.onChange != nil {
		b.onChange(from, to)
	}
}

func (b *Breaker) since(t time.Time) int64 { return int64(t.Sub(b.origin)) }
