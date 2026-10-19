package limits

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSemaphore(t *testing.T) {
	s := NewSemaphore(3)
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
		"zero slots":         func() { NewSemaphore(0) },
		"release unacquired": func() { NewSemaphore(1).Release() },
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
	s := NewSemaphore(slots)
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

func TestSemaphoreAllocs(t *testing.T) {
	s := NewSemaphore(1)
	allocs := testing.AllocsPerRun(100, func() {
		if s.TryAcquire() {
			s.Release()
		}
	})
	if allocs != 0 {
		t.Errorf("TryAcquire and Release allocate %v times, want 0", allocs)
	}
}

func BenchmarkSemaphore(b *testing.B) {
	b.ReportAllocs()
	s := NewSemaphore(1 << 20)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if s.TryAcquire() {
				s.Release()
			}
		}
	})
}
