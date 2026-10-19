package balancer

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MuditGarg007/prism/internal/upstream"
)

func withWeights(ws ...int) []*upstream.Backend {
	bs := backends(len(ws))
	for i, w := range ws {
		bs[i].Weight = w
	}
	return bs
}

// sequence returns n picks as letters, backend 0 being "a".
func sequence(t *testing.T, bal upstream.Balancer, bs []*upstream.Backend, c upstream.Choice, n int) string {
	t.Helper()
	var sb strings.Builder
	for range n {
		b, err := bal.Pick(c)
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteByte(byte('a' + indexOf(bs, b)))
	}
	return sb.String()
}

func TestWeightedSequence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		weights []int
		down    []int
		want    string
	}{
		// The example from nginx's upstream module: smooth, not aaaaabc.
		{"nginx example", []int{5, 1, 1}, nil, "aabacaa" + "aabacaa"},
		{"equal weights", []int{1, 1, 1}, nil, "abcabc"},
		{"zero weight counts as one", []int{0, 0}, nil, "abab"},
		{"two to one", []int{2, 1}, nil, "aba" + "aba"},
		{"heavy backend down", []int{5, 1, 1}, []int{0}, "bcbcbc"},
		{"light backend down", []int{5, 1, 1}, []int{1}, "aaacaa" + "aaacaa"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bs := withWeights(tt.weights...)
			for _, i := range tt.down {
				bs[i].SetHealthy(false)
			}
			got := sequence(t, NewWeighted(bs), bs, upstream.Choice{}, len(tt.want))
			if got != tt.want {
				t.Errorf("picks = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestWeightedDistribution checks that every full cycle gives each backend
// exactly its weight, in the same order every cycle.
func TestWeightedDistribution(t *testing.T) {
	for _, weights := range [][]int{{1}, {1, 1, 1}, {3, 1}, {5, 1, 1}, {10, 3, 2, 1}, {100, 1}} {
		bs := withWeights(weights...)
		total := 0
		for _, w := range weights {
			total += w
		}
		const cycles = 50
		seq := sequence(t, NewWeighted(bs), bs, upstream.Choice{}, cycles*total)
		for i, w := range weights {
			if got := strings.Count(seq, string(rune('a'+i))); got != cycles*w {
				t.Errorf("weights %v: backend %d got %d picks, want %d", weights, i, got, cycles*w)
			}
		}
		// Each cycle is the same sequence, so the picks repeat exactly.
		if first := seq[:total]; seq != strings.Repeat(first, cycles) {
			t.Errorf("weights %v: cycles differ", weights)
		}
	}
}

func TestWeightedSkipsTried(t *testing.T) {
	bs := withWeights(5, 1, 1)
	c := upstream.Choice{Tried: bs[:1]}
	if got, want := sequence(t, NewWeighted(bs), bs, c, 6), "bcbcbc"; got != want {
		t.Errorf("picks = %s, want %s", got, want)
	}
}

func TestWeightedNoBackend(t *testing.T) {
	down := withWeights(1, 2)
	for _, b := range down {
		b.SetHealthy(false)
	}
	all := withWeights(1, 2)
	for _, tt := range []struct {
		name string
		bs   []*upstream.Backend
		c    upstream.Choice
	}{
		{"empty", nil, upstream.Choice{}},
		{"all down", down, upstream.Choice{}},
		{"all tried", all, upstream.Choice{Tried: all}},
	} {
		if _, err := NewWeighted(tt.bs).Pick(tt.c); !errors.Is(err, upstream.ErrNoBackend) {
			t.Errorf("%s: got %v, want ErrNoBackend", tt.name, err)
		}
	}
}

// TestWeightedConcurrent checks that concurrent picks still give each
// backend exactly its share: the lock keeps one global sequence.
func TestWeightedConcurrent(t *testing.T) {
	const goroutines, picks = 8, 700 // 8*700 is a multiple of the total weight 7
	bs := withWeights(5, 1, 1)
	w := NewWeighted(bs)
	counts := make([][3]int, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for range picks {
				b, err := w.Pick(upstream.Choice{})
				if err != nil {
					t.Error(err)
					return
				}
				counts[g][indexOf(bs, b)]++
			}
		})
	}
	wg.Wait()
	var total [3]int
	for _, c := range counts {
		for i, n := range c {
			total[i] += n
		}
	}
	cycles := goroutines * picks / 7
	for i, want := range []int{5 * cycles, cycles, cycles} {
		if total[i] != want {
			t.Errorf("backend %d got %d picks, want %d", i, total[i], want)
		}
	}
}

func TestWeightedNoAllocs(t *testing.T) {
	w := NewWeighted(withWeights(5, 1, 1))
	if n := testing.AllocsPerRun(100, func() { w.Pick(upstream.Choice{}) }); n != 0 {
		t.Errorf("Pick allocates %v times, want 0", n)
	}
}

func BenchmarkWeighted(b *testing.B) {
	w := NewWeighted(withWeights(8, 4, 2, 1, 1, 1, 1, 1))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w.Pick(upstream.Choice{})
		}
	})
}
