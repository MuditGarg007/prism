// Package limits caps how much work a route takes on at once.
package limits

// Semaphore caps the requests a route serves at the same time. A slot is a
// value in a buffered channel, so M3 can add a queue that waits for one with
// a select on the request's context.
type Semaphore struct {
	slots chan struct{}
}

// NewSemaphore returns a Semaphore with n slots. n must be at least 1.
func NewSemaphore(n int) *Semaphore {
	if n < 1 {
		panic("limits: semaphore needs at least one slot")
	}
	return &Semaphore{slots: make(chan struct{}, n)}
}

// TryAcquire takes a slot if one is free and reports whether it did. It never
// waits: a route at its cap answers at once rather than letting requests pile
// up behind it.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release gives back a slot taken by TryAcquire.
func (s *Semaphore) Release() {
	select {
	case <-s.slots:
	default:
		panic("limits: Release without a matching acquire")
	}
}

// InUse returns how many slots are taken.
func (s *Semaphore) InUse() int { return len(s.slots) }

// Cap returns the number of slots.
func (s *Semaphore) Cap() int { return cap(s.slots) }
