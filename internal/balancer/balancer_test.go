package balancer

import (
	"errors"
	"net/http/httptest"
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
	rr := &RoundRobin{}
	req := httptest.NewRequest("GET", "/", nil)
	for i := range 7 {
		b, err := rr.Pick(req, bs)
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
	rr := &RoundRobin{}
	req := httptest.NewRequest("GET", "/", nil)
	for range 9 {
		b, err := rr.Pick(req, bs)
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
	req := httptest.NewRequest("GET", "/", nil)
	for name, bs := range map[string][]*upstream.Backend{"empty": nil, "all down": down} {
		if _, err := (&RoundRobin{}).Pick(req, bs); !errors.Is(err, upstream.ErrNoBackend) {
			t.Errorf("%s: got %v, want ErrNoBackend", name, err)
		}
	}
}

// TestRoundRobinConcurrent checks that concurrent picks split exactly
// evenly: every counter value maps to one backend.
func TestRoundRobinConcurrent(t *testing.T) {
	const goroutines, picks = 8, 3000
	bs := backends(3)
	rr := &RoundRobin{}
	counts := make([][3]int, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			req := httptest.NewRequest("GET", "/", nil)
			for range picks {
				b, err := rr.Pick(req, bs)
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
	bs := backends(8)
	rr := &RoundRobin{}
	req := httptest.NewRequest("GET", "/", nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rr.Pick(req, bs)
		}
	})
}
