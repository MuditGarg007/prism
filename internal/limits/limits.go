// Package limits caps how much work a route takes on at once.
package limits

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

var (
	// ErrQueueFull is returned by Acquire when no slot is free and the queue
	// already holds as many waiters as it may.
	ErrQueueFull = errors.New("limits: queue full")
	// ErrQueueTimeout is returned by Acquire when no slot came free in time.
	ErrQueueTimeout = errors.New("limits: timed out waiting for a slot")
)

// Semaphore caps the requests a route serves at the same time, and how many
// more may wait for a slot. A slot is a value in a buffered channel, so a
// waiter selects on the channel, its context and a timer, and Go hands freed
// slots to waiters in the order they started waiting.
type Semaphore struct {
	slots   chan struct{}
	queue   int64
	waiting atomic.Int64
}

// NewSemaphore returns a Semaphore with n slots, where up to queue requests
// may wait for one. n must be at least 1; a queue of 0 means none waits.
func NewSemaphore(n, queue int) *Semaphore {
	if n < 1 {
		panic("limits: semaphore needs at least one slot")
	}
	return &Semaphore{slots: make(chan struct{}, n), queue: int64(queue)}
}

// TryAcquire takes a slot if one is free and reports whether it did. It never
// waits.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Acquire takes a slot, waiting up to timeout for one if none is free. It
// fails at once with ErrQueueFull when the queue is full, so a bounded
// number of requests pile up behind a busy route, and with ctx's error if
// ctx is done first. A free slot never costs an allocation.
func (s *Semaphore) Acquire(ctx context.Context, timeout time.Duration) error {
	if s.TryAcquire() {
		return nil
	}
	// A CAS rather than Add and undo, so Waiting never reads over the cap.
	for {
		n := s.waiting.Load()
		if n >= s.queue {
			return ErrQueueFull
		}
		if s.waiting.CompareAndSwap(n, n+1) {
			break
		}
	}
	defer s.waiting.Add(-1)
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-t.C:
		return ErrQueueTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release gives back a slot taken by TryAcquire or Acquire.
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

// Waiting returns how many requests are waiting for a slot.
func (s *Semaphore) Waiting() int { return int(s.waiting.Load()) }
