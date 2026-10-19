package balancer

import (
	"math/rand/v2"

	"github.com/MuditGarg007/prism/internal/upstream"
)

// LeastConn sends each request to the less loaded of two backends chosen at
// random ("power of two choices"), where load is in-flight requests divided
// by weight. Comparing two random backends instead of scanning for the
// global minimum is O(1), and it avoids the herd where every concurrent pick
// sees the same idle backend before any of them has started a request on
// it. The result is close to true least-connections; Envoy's least_request
// works the same way.
type LeastConn struct {
	bs   []*upstream.Backend
	intN func(n int) int // random int in [0, n); swapped in tests
}

// NewLeastConn returns a power-of-two-choices least-connections balancer
// over bs.
func NewLeastConn(bs []*upstream.Backend) upstream.Balancer {
	return &LeastConn{bs: bs, intN: rand.IntN}
}

func (lc *LeastConn) Pick(c upstream.Choice) (*upstream.Backend, error) {
	// Fast path: two distinct random backends, both usable. When either is
	// not, the slow path samples again from the usable ones only; every
	// usable pair is still equally likely overall.
	if n := len(lc.bs); n >= 2 {
		i := lc.intN(n)
		j := lc.intN(n - 1)
		if j >= i {
			j++
		}
		if a, b := lc.bs[i], lc.bs[j]; c.Usable(a) && c.Usable(b) {
			return lc.lighter(a, b), nil
		}
	}
	return lc.pickUsable(c)
}

// pickUsable draws two of the usable backends uniformly in one pass
// (reservoir sampling), so a health change during the scan cannot leave it
// with a stale count.
func (lc *LeastConn) pickUsable(c upstream.Choice) (*upstream.Backend, error) {
	var pair [2]*upstream.Backend
	seen := 0
	for _, b := range lc.bs {
		if !c.Usable(b) {
			continue
		}
		if seen < 2 {
			pair[seen] = b
		} else if r := lc.intN(seen + 1); r < 2 {
			pair[r] = b
		}
		seen++
	}
	switch seen {
	case 0:
		return nil, upstream.ErrNoBackend
	case 1:
		return pair[0], nil
	}
	return lc.lighter(pair[0], pair[1]), nil
}

// lighter returns whichever of a and b has fewer in-flight requests per unit
// of weight, comparing (inflight+1)/weight by cross-multiplying. The +1
// counts the request being placed, so between idle backends the heavier one
// wins. Ties are broken at random.
func (lc *LeastConn) lighter(a, b *upstream.Backend) *upstream.Backend {
	la := (a.Inflight() + 1) * int64(weight(b))
	lb := (b.Inflight() + 1) * int64(weight(a))
	switch {
	case la < lb:
		return a
	case lb < la:
		return b
	case lc.intN(2) == 0:
		return a
	default:
		return b
	}
}
