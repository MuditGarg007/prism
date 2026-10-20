package resilience

import (
	"sync"
	"testing"
	"time"
)

func newTestBudget(ratio float64, minPerSec int) (*Budget, *time.Time) {
	b := NewBudget(ratio, minPerSec)
	now := time.Unix(1000, 0)
	b.w = NewWindow(BudgetWindow, now)
	b.now = func() time.Time { return now }
	return b, &now
}

// retries counts how many retries in a row b allows, up to limit.
func retries(b *Budget, limit int) int {
	n := 0
	for n < limit && b.TryRetry() {
		n++
	}
	return n
}

func TestBudgetFloor(t *testing.T) {
	// With no requests, only the floor is left: 1/s over the 10 s window.
	b, _ := newTestBudget(0.2, 1)
	if got := retries(b, 100); got != 10 {
		t.Errorf("idle route got %d retries, want 10", got)
	}
}

func TestBudgetRatio(t *testing.T) {
	b, _ := newTestBudget(0.2, 1)
	for range 100 {
		b.Request()
	}
	// 0.2 × 100 requests + 10 from the floor.
	if got := retries(b, 1000); got != 30 {
		t.Errorf("got %d retries, want 30", got)
	}
	// Retries are not requests, so they earn nothing.
	if b.TryRetry() {
		t.Error("retry allowed past the budget")
	}
	for range 5 {
		b.Request()
	}
	if got := retries(b, 1000); got != 1 {
		t.Errorf("5 more requests gave %d retries, want 1", got)
	}
}

func TestBudgetSlides(t *testing.T) {
	b, now := newTestBudget(0.5, 1)
	for range 20 {
		b.Request()
	}
	if got := retries(b, 1000); got != 20 {
		t.Fatalf("got %d retries, want 20", got)
	}
	// Half a window on, everything is still counted.
	*now = now.Add(BudgetWindow / 2)
	if b.TryRetry() {
		t.Error("retry allowed while the spent budget is still in the window")
	}
	// A whole window on, the old requests and retries are both gone.
	*now = now.Add(BudgetWindow)
	if got := retries(b, 1000); got != 10 {
		t.Errorf("after the window slid, got %d retries, want the floor of 10", got)
	}
}

// TestBudgetAmplification drives a route where every attempt fails and
// every request wants 3 retries, and checks that the attempts per request
// stay near 1 + ratio instead of 4.
func TestBudgetAmplification(t *testing.T) {
	b, now := newTestBudget(0.2, 1)
	var reqs, attempts int
	for range 100 { // 100 s
		for range 1000 { // 1000 requests a second
			b.Request()
			reqs++
			attempts++
			for range 3 {
				if !b.TryRetry() {
					break
				}
				attempts++
			}
		}
		*now = now.Add(time.Second)
	}
	if got := float64(attempts) / float64(reqs); got > 1.22 {
		t.Errorf("%.3f attempts per request, want at most 1.22", got)
	}
}

func TestBudgetConcurrent(t *testing.T) {
	b, _ := newTestBudget(0.1, 1)
	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 500 {
				b.Request()
				if b.TryRetry() {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	// 0.1 × 4000 + 10, never more however the goroutines interleave.
	if allowed > 410 {
		t.Errorf("%d retries allowed, budget is 410", allowed)
	}
}

// FuzzBudget checks Budget against a plain list of the requests and retries
// it has seen. Each byte is one call: the low bit picks Request or TryRetry,
// and the rest is how far the clock moves first, in 50 ms steps.
func FuzzBudget(f *testing.F) {
	f.Add([]byte{0, 1, 1, 1, 0, 1, 1, 255, 1, 1})
	f.Add([]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 201, 1, 1})
	f.Fuzz(func(t *testing.T, ops []byte) {
		const ratio, minPerSec = 0.25, 1
		b, now := newTestBudget(ratio, minPerSec)
		start := *now
		width := BudgetWindow / windowBuckets
		type event struct {
			bucket int64
			retry  bool
		}
		var events []event
		for i, op := range ops {
			*now = now.Add(time.Duration(op>>1) * 50 * time.Millisecond)
			bucket := int64(now.Sub(start) / width)
			if op&1 == 0 {
				b.Request()
				events = append(events, event{bucket, false})
				continue
			}
			var reqs, rets int
			for _, e := range events {
				if e.bucket > bucket-windowBuckets {
					if e.retry {
						rets++
					} else {
						reqs++
					}
				}
			}
			want := float64(rets+1) <= ratio*float64(reqs)+minPerSec*BudgetWindow.Seconds()
			if got := b.TryRetry(); got != want {
				t.Fatalf("call %d: TryRetry = %v with %d requests and %d retries in the window, want %v", i, got, reqs, rets, want)
			}
			if want {
				events = append(events, event{bucket, true})
			}
		}
	})
}

func BenchmarkBudget(b *testing.B) {
	bu := NewBudget(0.2, 10)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bu.Request()
		}
	})
}
