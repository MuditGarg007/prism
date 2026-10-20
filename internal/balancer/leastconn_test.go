package balancer

import (
	"errors"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/MuditGarg007/prism/internal/upstream"
)

// seededLeastConn returns a LeastConn with a fixed random sequence. It is
// not safe for concurrent use.
func seededLeastConn(bs []*upstream.Backend) *LeastConn {
	lc := NewLeastConn(bs).(*LeastConn)
	lc.intN = rand.New(rand.NewPCG(1, 2)).IntN
	return lc
}

func load(b *upstream.Backend, n int) {
	for range n {
		b.Acquire()
	}
}

func TestLeastConnLighter(t *testing.T) {
	for _, tt := range []struct {
		name              string
		wa, ia, wb, ib    int
		wantA, wantEither bool
	}{
		{name: "fewer in flight wins", wa: 1, ia: 1, wb: 1, ib: 3, wantA: true},
		{name: "more in flight loses", wa: 1, ia: 4, wb: 1, ib: 0},
		{name: "idle: heavier wins", wa: 3, ia: 0, wb: 1, ib: 0, wantA: true},
		{name: "per weight: 2/2 beats 1/1", wa: 2, ia: 1, wb: 1, ib: 1, wantA: true},
		{name: "per weight: 4/2 loses to 1/1", wa: 2, ia: 4, wb: 1, ib: 1},
		{name: "equal per weight is a tie", wa: 2, ia: 3, wb: 1, ib: 1, wantEither: true},
		{name: "zero weight counts as one", wa: 0, ia: 0, wb: 1, ib: 1, wantA: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, b := upstream.NewBackend(nil, tt.wa), upstream.NewBackend(nil, tt.wb)
			load(a, tt.ia)
			load(b, tt.ib)
			lc := seededLeastConn(nil)
			gotA := 0
			for range 1000 {
				if lc.lighter(a, b) == a {
					gotA++
				}
			}
			switch {
			case tt.wantEither:
				if gotA < 400 || gotA > 600 {
					t.Errorf("a won %d of 1000 ties, want about half", gotA)
				}
			case tt.wantA && gotA != 1000:
				t.Errorf("a won %d of 1000, want all", gotA)
			case !tt.wantA && gotA != 0:
				t.Errorf("a won %d of 1000, want none", gotA)
			}
		})
	}
}

// TestLeastConnEven checks that idle, equal backends share picks evenly,
// on both the fast path and the path that skips unusable backends.
func TestLeastConnEven(t *testing.T) {
	for _, tt := range []struct {
		name string
		n    int
		down []int
	}{
		{"all usable", 4, nil},
		{"one down", 4, []int{1}},
		{"two down", 5, []int{0, 3}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bs := backends(tt.n)
			for _, i := range tt.down {
				bs[i].SetHealthy(false)
			}
			lc := seededLeastConn(bs)
			const picks = 40000
			counts := make([]int, tt.n)
			for range picks {
				b, err := lc.Pick(upstream.Choice{})
				if err != nil {
					t.Fatal(err)
				}
				counts[indexOf(bs, b)]++
			}
			want := picks / (tt.n - len(tt.down))
			for i, n := range counts {
				if !bs[i].Available() {
					if n != 0 {
						t.Errorf("unavailable backend %d got %d picks", i, n)
					}
					continue
				}
				if n < want*95/100 || n > want*105/100 {
					t.Errorf("backend %d got %d picks, want %d ± 5%%", i, n, want)
				}
			}
		})
	}
}

// TestLeastConnAvoidsLoaded checks that the most loaded backend is never
// picked while every other one is lighter: it loses every pair it is in.
func TestLeastConnAvoidsLoaded(t *testing.T) {
	bs := backends(4)
	load(bs[2], 10)
	load(bs[0], 1)
	lc := seededLeastConn(bs)
	for range 10000 {
		if b, _ := lc.Pick(upstream.Choice{}); b == bs[2] {
			t.Fatal("picked the most loaded backend")
		}
	}
}

// simulate sends perStep requests every step for steps steps. A request on
// backend i finishes duration[i] steps after it starts. It returns the
// share of picks each backend got.
func simulate(t *testing.T, bs []*upstream.Backend, duration []int, perStep, steps int) []float64 {
	t.Helper()
	lc := seededLeastConn(bs)
	finish := make(map[int][]*upstream.Backend)
	counts := make([]int, len(bs))
	for step := range steps {
		for _, b := range finish[step] {
			b.Release()
		}
		delete(finish, step)
		for range perStep {
			b, err := lc.Pick(upstream.Choice{})
			if err != nil {
				t.Fatal(err)
			}
			b.Acquire()
			i := indexOf(bs, b)
			counts[i]++
			finish[step+duration[i]] = append(finish[step+duration[i]], b)
		}
	}
	share := make([]float64, len(bs))
	for i, n := range counts {
		share[i] = float64(n) / float64(perStep*steps)
	}
	return share
}

func TestLeastConnLoadShares(t *testing.T) {
	for _, tt := range []struct {
		name     string
		weights  []int
		duration []int
		want     []float64 // expected share of picks
	}{
		// Equal in-flight counts mean picks in inverse proportion to
		// latency: 1/2 : 1/8 is 80% : 20%. Round robin would give 50%.
		{"slow backend gets less", []int{1, 1}, []int{2, 8}, []float64{0.8, 0.2}},
		{"weights with equal latency", []int{3, 1}, []int{4, 4}, []float64{0.75, 0.25}},
		{"equal backends", []int{1, 1, 1}, []int{3, 3, 3}, []float64{1.0 / 3, 1.0 / 3, 1.0 / 3}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			share := simulate(t, withWeights(tt.weights...), tt.duration, 10, 2000)
			for i, want := range tt.want {
				if d := share[i] - want; d < -0.05 || d > 0.05 {
					t.Errorf("backend %d got %.3f of picks, want %.3f ± 0.05", i, share[i], want)
				}
			}
		})
	}
}

func TestLeastConnSingleAndNone(t *testing.T) {
	bs := backends(3)
	bs[0].SetHealthy(false)
	c := upstream.Choice{Tried: bs[1:2]}
	for range 100 {
		if b, err := NewLeastConn(bs).Pick(c); err != nil || b != bs[2] {
			t.Fatalf("Pick = %v, %v; want the only usable backend", b, err)
		}
	}
	if b, err := NewLeastConn(bs[2:]).Pick(upstream.Choice{}); err != nil || b != bs[2] {
		t.Errorf("one-backend pool: Pick = %v, %v", b, err)
	}

	down := backends(2)
	for _, b := range down {
		b.SetHealthy(false)
	}
	all := backends(3)
	for _, tt := range []struct {
		name string
		bs   []*upstream.Backend
		c    upstream.Choice
	}{
		{"empty", nil, upstream.Choice{}},
		{"all down", down, upstream.Choice{}},
		{"all tried", all, upstream.Choice{Tried: all}},
	} {
		if _, err := NewLeastConn(tt.bs).Pick(tt.c); !errors.Is(err, upstream.ErrNoBackend) {
			t.Errorf("%s: got %v, want ErrNoBackend", tt.name, err)
		}
	}
}

// TestLeastConnConcurrent runs picks with real Acquire/Release from many
// goroutines, for the race detector, and checks every request is released.
func TestLeastConnConcurrent(t *testing.T) {
	bs := backends(4)
	lc := NewLeastConn(bs)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 2000 {
				b, err := lc.Pick(upstream.Choice{})
				if err != nil {
					t.Error(err)
					return
				}
				b.Acquire()
				b.Release()
			}
		})
	}
	wg.Wait()
	for i, b := range bs {
		if n := b.Inflight(); n != 0 {
			t.Errorf("backend %d has %d in flight, want 0", i, n)
		}
	}
}

func TestLeastConnNoAllocs(t *testing.T) {
	bs := backends(4)
	bs[1].SetHealthy(false) // exercise the slow path too
	lc := NewLeastConn(bs)
	if n := testing.AllocsPerRun(100, func() { lc.Pick(upstream.Choice{}) }); n != 0 {
		t.Errorf("Pick allocates %v times, want 0", n)
	}
}

func BenchmarkLeastConn(b *testing.B) {
	lc := NewLeastConn(backends(8))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lc.Pick(upstream.Choice{})
		}
	})
}
