package limits

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemaphore(t *testing.T) {
	s := NewSemaphore(3, 0)
	for i := range 3 {
		if !s.TryAcquire() {
			t.Fatalf("acquire %d of 3 failed", i)
		}
	}
	if s.TryAcquire() {
		t.Fatal("acquired a fourth slot of 3")
	}
	if s.InUse() != 3 || s.Cap() != 3 {
		t.Errorf("in use %d of %d, want 3 of 3", s.InUse(), s.Cap())
	}
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("no slot after a release")
	}
	if s.TryAcquire() {
		t.Fatal("a release freed more than one slot")
	}
	for range 3 {
		s.Release()
	}
	if s.InUse() != 0 {
		t.Errorf("in use %d after releasing everything", s.InUse())
	}
}

func TestSemaphorePanics(t *testing.T) {
	for name, f := range map[string]func(){
		"zero slots":         func() { NewSemaphore(0, 0) },
		"release unacquired": func() { NewSemaphore(1, 0).Release() },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("did not panic")
				}
			}()
			f()
		})
	}
}

// TestSemaphoreConcurrent checks under -race that the cap holds while many
// goroutines take and give back slots, and that every slot comes back.
func TestSemaphoreConcurrent(t *testing.T) {
	const slots = 4
	s := NewSemaphore(slots, 0)
	var held, peak, acquired atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 2000 {
				if !s.TryAcquire() {
					continue
				}
				acquired.Add(1)
				n := held.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				held.Add(-1)
				s.Release()
			}
		})
	}
	wg.Wait()
	if p := peak.Load(); p > slots {
		t.Errorf("%d slots held at once, cap is %d", p, slots)
	}
	if acquired.Load() == 0 {
		t.Error("no goroutine ever got a slot")
	}
	if s.InUse() != 0 {
		t.Errorf("in use %d after all goroutines finished", s.InUse())
	}
}

// waitWaiting waits until n requests are queued on s.
func waitWaiting(t *testing.T, s *Semaphore, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); s.Waiting() != n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting, want %d", s.Waiting(), n)
		}
	}
}

func TestSemaphoreQueue(t *testing.T) {
	ctx := context.Background()
	s := NewSemaphore(1, 2)
	if err := s.Acquire(ctx, time.Hour); err != nil {
		t.Fatalf("free slot: %v", err)
	}

	got := make(chan error, 2)
	for range 2 {
		go func() { got <- s.Acquire(ctx, time.Hour) }()
	}
	waitWaiting(t, s, 2)
	if err := s.Acquire(ctx, time.Hour); !errors.Is(err, ErrQueueFull) {
		t.Errorf("third waiter got %v, want ErrQueueFull", err)
	}
	if s.Waiting() != 2 {
		t.Errorf("a refused waiter changed the queue to %d", s.Waiting())
	}

	// Each release hands the slot to exactly one waiter.
	for i := range 2 {
		s.Release()
		if err := <-got; err != nil {
			t.Fatalf("waiter %d: %v", i, err)
		}
		if s.InUse() != 1 {
			t.Fatalf("in use %d after a handoff", s.InUse())
		}
	}
	waitWaiting(t, s, 0)
	s.Release()
}

func TestSemaphoreQueueTimeout(t *testing.T) {
	s := NewSemaphore(1, 1)
	s.TryAcquire()
	start := time.Now()
	if err := s.Acquire(context.Background(), 10*time.Millisecond); !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("got %v, want ErrQueueTimeout", err)
	}
	if d := time.Since(start); d < 10*time.Millisecond {
		t.Errorf("gave up after %v, before the timeout", d)
	}
	if s.Waiting() != 0 || s.InUse() != 1 {
		t.Errorf("waiting %d, in use %d after a timeout", s.Waiting(), s.InUse())
	}
}

func TestSemaphoreQueueCanceled(t *testing.T) {
	s := NewSemaphore(1, 1)
	s.TryAcquire()
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error)
	go func() { got <- s.Acquire(ctx, time.Hour) }()
	waitWaiting(t, s, 1)
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if s.Waiting() != 0 || s.InUse() != 1 {
		t.Errorf("waiting %d, in use %d after a cancel", s.Waiting(), s.InUse())
	}
}

func TestSemaphoreNoQueue(t *testing.T) {
	s := NewSemaphore(1, 0)
	s.TryAcquire()
	if err := s.Acquire(context.Background(), time.Hour); !errors.Is(err, ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

// TestSemaphoreQueueConcurrent checks under -race that neither the slots nor
// the queue ever go over their caps, and that everything is given back.
func TestSemaphoreQueueConcurrent(t *testing.T) {
	const slots, queue = 2, 3
	s := NewSemaphore(slots, queue)
	var held, peakHeld, peakWait atomic.Int64
	raise := func(peak *atomic.Int64, n int64) {
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 300 {
				raise(&peakWait, int64(s.Waiting()))
				if s.Acquire(context.Background(), time.Millisecond) != nil {
					continue
				}
				raise(&peakHeld, held.Add(1))
				held.Add(-1)
				s.Release()
			}
		})
	}
	wg.Wait()
	if p := peakHeld.Load(); p > slots {
		t.Errorf("%d slots held at once, cap is %d", p, slots)
	}
	if p := peakWait.Load(); p > queue {
		t.Errorf("%d waiting at once, queue is %d", p, queue)
	}
	if s.InUse() != 0 || s.Waiting() != 0 {
		t.Errorf("in use %d, waiting %d at the end", s.InUse(), s.Waiting())
	}
}

func TestSemaphoreAllocs(t *testing.T) {
	s := NewSemaphore(1, 0)
	allocs := testing.AllocsPerRun(100, func() {
		if s.TryAcquire() {
			s.Release()
		}
	})
	if allocs != 0 {
		t.Errorf("TryAcquire and Release allocate %v times, want 0", allocs)
	}
	ctx := context.Background()
	allocs = testing.AllocsPerRun(100, func() {
		if s.Acquire(ctx, time.Second) == nil {
			s.Release()
		}
	})
	if allocs != 0 {
		t.Errorf("Acquire of a free slot allocates %v times, want 0", allocs)
	}
}

func BenchmarkSemaphore(b *testing.B) {
	b.ReportAllocs()
	s := NewSemaphore(1<<20, 0)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if s.TryAcquire() {
				s.Release()
			}
		}
	})
}
