package resilience

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestWindowSlides(t *testing.T) {
	// A 10s span has 1s buckets.
	type step struct {
		at   time.Duration // since t0
		add  Counts
		want Counts // Sum right after the add
	}
	steps := []struct {
		name string
		step
	}{
		{"first event", step{0, Counts{1, 0}, Counts{1, 0}}},
		{"same bucket", step{900 * time.Millisecond, Counts{1, 1}, Counts{2, 1}}},
		{"later bucket", step{5 * time.Second, Counts{3, 2}, Counts{5, 3}}},
		{"last moment of first bucket's life", step{9*time.Second + 999*time.Millisecond, Counts{}, Counts{5, 3}}},
		{"first bucket expires", step{10 * time.Second, Counts{}, Counts{3, 2}}},
		{"5s bucket still in", step{14*time.Second + 999*time.Millisecond, Counts{1, 0}, Counts{4, 2}}},
		{"5s bucket expires", step{15 * time.Second, Counts{}, Counts{1, 0}}},
		{"gap longer than span", step{60 * time.Second, Counts{2, 2}, Counts{2, 2}}},
	}
	w := NewWindow(10*time.Second, t0)
	for _, s := range steps {
		now := t0.Add(s.at)
		w.Add(now, s.add)
		if got := w.Sum(now); got != s.want {
			t.Fatalf("%s: Sum = %+v, want %+v", s.name, got, s.want)
		}
	}
}

func TestWindowTimeBeforeHead(t *testing.T) {
	w := NewWindow(10*time.Second, t0)
	w.Add(t0.Add(5*time.Second), Counts{1, 0})
	// An older time, as from a caller that read the clock earlier, counts in
	// the newest bucket instead of being lost or landing in a stale one.
	w.Add(t0.Add(time.Second), Counts{1, 1})
	w.Add(t0.Add(-time.Hour), Counts{1, 0})
	if got, want := w.Sum(t0.Add(14*time.Second)), (Counts{3, 1}); got != want {
		t.Errorf("Sum before the head bucket expires = %+v, want %+v", got, want)
	}
	if got := w.Sum(t0.Add(15 * time.Second)); got != (Counts{}) {
		t.Errorf("Sum after it expires = %+v, want empty", got)
	}
}

func TestWindowReset(t *testing.T) {
	w := NewWindow(10*time.Second, t0)
	w.Add(t0, Counts{5, 5})
	w.Add(t0.Add(3*time.Second), Counts{5, 5})
	w.Reset()
	if got := w.Sum(t0.Add(3 * time.Second)); got != (Counts{}) {
		t.Fatalf("Sum after Reset = %+v, want empty", got)
	}
	w.Add(t0.Add(4*time.Second), Counts{1, 0})
	// Nothing from before the reset may come back, or be subtracted twice,
	// when its bucket is cleared.
	if got, want := w.Sum(t0.Add(13*time.Second)), (Counts{1, 0}); got != want {
		t.Errorf("Sum = %+v, want %+v", got, want)
	}
	if got := w.Sum(t0.Add(14 * time.Second)); got != (Counts{}) {
		t.Errorf("Sum after expiry = %+v, want empty", got)
	}
}

func TestWindowTinySpan(t *testing.T) {
	// A span shorter than windowBuckets nanoseconds must not divide by zero.
	w := NewWindow(5, t0)
	w.Add(t0, Counts{1, 0})
	if got := w.Sum(t0.Add(windowBuckets)); got != (Counts{}) {
		t.Errorf("Sum = %+v, want empty", got)
	}
}

// FuzzWindow checks a Window against a plain list of events: an event counts
// while its bucket is one of the windowBuckets newest.
func FuzzWindow(f *testing.F) {
	f.Add([]byte{0, 1, 50, 99, 100, 101, 255, 3})
	f.Add([]byte{10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10})
	f.Fuzz(func(t *testing.T, gaps []byte) {
		const width = 100 * time.Millisecond
		w := NewWindow(windowBuckets*width, t0)
		type event struct {
			bucket int64
			c      Counts
		}
		var events []event
		var at time.Duration
		for i, g := range gaps {
			at += time.Duration(g) * 10 * time.Millisecond
			now := t0.Add(at)
			c := Counts{int64(i%3 + 1), int64(i % 2)}
			w.Add(now, c)
			bucket := int64(at / width)
			events = append(events, event{bucket, c})
			var want Counts
			for _, e := range events {
				if e.bucket > bucket-windowBuckets {
					want.add(e.c)
				}
			}
			if got := w.Sum(now); got != want {
				t.Fatalf("event %d at %v: Sum = %+v, want %+v", i, at, got, want)
			}
		}
	})
}

func BenchmarkWindowAdd(b *testing.B) {
	w := NewWindow(10*time.Second, t0)
	now := t0
	for b.Loop() {
		// 1ms a step, so the loop also crosses buckets.
		now = now.Add(time.Millisecond)
		w.Add(now, Counts{1, 0})
	}
}
