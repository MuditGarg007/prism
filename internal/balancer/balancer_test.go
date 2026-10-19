package balancer

import (
	"errors"
	"sync"
	"testing"

	"github.com/MuditGarg007/prism/internal/upstream"
)

func backends(n int) []*upstream.Backend {
	bs := make([]*upstream.Backend, n)
	for i := range bs {
		bs[i] = &upstream.Backend{}
	}
	return bs
}

func indexOf(bs []*upstream.Backend, b *upstream.Backend) int {
	for i := range bs {
		if bs[i] == b {
			return i
		}
	}
	return -1
}

func TestRoundRobinOrder(t *testing.T) {
	bs := backends(3)
	rr := NewRoundRobin(bs)
	for i := range 7 {
		b, err := rr.Pick(upstream.Choice{})
		if err != nil {
			t.Fatal(err)
		}
		if got := indexOf(bs, b); got != i%3 {
			t.Errorf("pick %d = backend %d, want %d", i, got, i%3)
		}
	}
}

func TestRoundRobinSkipsUnavailable(t *testing.T) {
	bs := backends(3)
	bs[1].SetHealthy(false)
	rr := NewRoundRobin(bs)
	for range 9 {
		b, err := rr.Pick(upstream.Choice{})
		if err != nil {
			t.Fatal(err)
		}
		if b == bs[1] {
			t.Fatal("picked an unavailable backend")
		}
	}
}

func TestRoundRobinNoBackend(t *testing.T) {
	down := backends(2)
	for _, b := range down {
		b.SetHealthy(false)
	}
	all := backends(2)
	for _, tt := range []struct {
		name string
		bs   []*upstream.Backend
		c    upstream.Choice
	}{
		{"empty", nil, upstream.Choice{}},
		{"all down", down, upstream.Choice{}},
		{"all tried", all, upstream.Choice{Tried: all}},
	} {
		if _, err := NewRoundRobin(tt.bs).Pick(tt.c); !errors.Is(err, upstream.ErrNoBackend) {
			t.Errorf("%s: got %v, want ErrNoBackend", tt.name, err)
		}
	}
}

func TestRoundRobinSkipsTried(t *testing.T) {
	bs := backends(3)
	rr := NewRoundRobin(bs)
	c := upstream.Choice{Tried: bs[:1]}
	for range 9 {
		b, err := rr.Pick(c)
		if err != nil {
			t.Fatal(err)
		}
		if b == bs[0] {
			t.Fatal("picked a backend the request already tried")
		}
	}
}

// TestRoundRobinConcurrent checks that concurrent picks split exactly
// evenly: every counter value maps to one backend.
func TestRoundRobinConcurrent(t *testing.T) {
	const goroutines, picks = 8, 3000
	bs := backends(3)
	rr := NewRoundRobin(bs)
	counts := make([][3]int, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for range picks {
				b, err := rr.Pick(upstream.Choice{})
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
	want := goroutines * picks / 3
	for i, n := range total {
		if n != want {
			t.Errorf("backend %d got %d picks, want %d", i, n, want)
		}
	}
}

func BenchmarkRoundRobin(b *testing.B) {
	rr := NewRoundRobin(backends(8))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rr.Pick(upstream.Choice{})
		}
	})
}
