package resilience

import (
	"testing"
	"time"
	"unsafe"
)

var testOutlierConfig = OutlierConfig{
	ConsecutiveFailures: 3,
	BaseEjection:        30 * time.Second,
	MaxEjection:         75 * time.Second,
	MaxEjectionPercent:  50,
}

func newTestOutlier(n int, cfg OutlierConfig) (*Outlier, *fakeClock) {
	clk := &fakeClock{}
	o := NewOutlier(n, cfg)
	o.origin, o.now = t0, clk.now
	return o, clk
}

// fail reports n failures for member i and returns the last ejection time.
func fail(o *Outlier, i, n int) time.Duration {
	var d time.Duration
	for range n {
		d = o.Report(i, Failure)
	}
	return d
}

func TestOutlierEjectsAfterConsecutiveFailures(t *testing.T) {
	o, clk := newTestOutlier(4, testOutlierConfig)
	fail(o, 0, 2)
	o.Report(0, Success)
	if d := fail(o, 0, 2); d != 0 || o.Ejected(0) {
		t.Fatal("ejected although a success broke the streak")
	}
	o.Report(0, Neutral)
	if d := o.Report(0, Failure); d != 30*time.Second || !o.Ejected(0) {
		t.Fatalf("third failure in a row: ejected for %v, Ejected = %v; want 30s, true", d, o.Ejected(0))
	}
	if o.Ejected(1) {
		t.Error("a healthy member is ejected")
	}
	// Requests that were in flight keep failing; that must not extend it.
	if d := fail(o, 0, 3); d != 0 {
		t.Errorf("failures while ejected ejected it again for %v", d)
	}
	clk.advance(30*time.Second - 1)
	if !o.Ejected(0) {
		t.Fatal("back before the ejection ended")
	}
	clk.advance(1)
	if o.Ejected(0) {
		t.Fatal("still ejected after the ejection ended")
	}
	if o.members[0].until.Load() != 0 {
		t.Error("expired ejection not cleared")
	}
}

func TestOutlierEjectionGrowsAndDecays(t *testing.T) {
	o, clk := newTestOutlier(4, testOutlierConfig)
	steps := []struct {
		name  string
		clean time.Duration // time back in the pool before the next streak
		want  time.Duration
	}{
		{"first", 0, 30 * time.Second},
		{"second in a row", 0, 60 * time.Second},
		{"third is capped at max_ejection", 0, 75 * time.Second},
		{"one base of clean time forgives one", 30 * time.Second, 75 * time.Second},
		{"long recovery forgives everything", time.Hour, 30 * time.Second},
	}
	for _, s := range steps {
		clk.advance(s.clean)
		if d := fail(o, 0, 3); d != s.want {
			t.Fatalf("%s: ejected for %v, want %v", s.name, d, s.want)
		}
		clk.advance(s.want) // serve out the ejection
	}
}

func TestOutlierMaxEjectionPercent(t *testing.T) {
	tests := []struct {
		name    string
		members int
		percent int
		want    int // members that end up ejected when all fail
	}{
		{"pool of one is never ejected", 1, 50, 0},
		{"pool of two keeps one", 2, 50, 1},
		{"pool of ten at 50%", 10, 50, 5},
		{"pool of ten at 25%", 10, 25, 2},
		{"100% may empty the pool", 3, 100, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testOutlierConfig
			cfg.MaxEjectionPercent = tt.percent
			o, clk := newTestOutlier(tt.members, cfg)
			for i := range tt.members {
				fail(o, i, 3)
			}
			got := 0
			for i := range tt.members {
				if o.Ejected(i) {
					got++
				}
			}
			if got != tt.want {
				t.Fatalf("%d ejected, want %d", got, tt.want)
			}
			if tt.want == tt.members || tt.want == 0 {
				return
			}
			// Once the first ejections end, the members the cap held in
			// can go, after a new streak.
			clk.advance(30 * time.Second)
			if d := fail(o, tt.members-1, 3); d == 0 {
				t.Error("the cap still held a member in after the ejections ended")
			}
		})
	}
}

func TestOutlierMemberSize(t *testing.T) {
	if s := unsafe.Sizeof(member{}); s != 64 {
		t.Errorf("member is %d bytes, want 64", s)
	}
}

func BenchmarkOutlierSuccess(b *testing.B) {
	o := NewOutlier(4, testOutlierConfig)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !o.Ejected(1) {
				o.Report(1, Success)
			}
		}
	})
}
