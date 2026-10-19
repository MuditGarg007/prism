package balancer

import (
	"sync"

	"github.com/MuditGarg007/prism/internal/upstream"
)

// Weighted is nginx's smooth weighted round robin. Each pick adds every
// usable backend's weight to its current weight, picks the highest, and
// takes the total weight off the winner. Weights 5, 1, 1 give a a b a c a a
// instead of a burst of five a's, and over any full cycle each backend gets
// exactly its share.
//
// Only usable backends take part, so the share of one that is down or
// already tried spreads evenly over the rest.
type Weighted struct {
	bs []*upstream.Backend

	mu  sync.Mutex
	cur []int // current weight of bs[i]
}

// NewWeighted returns a smooth weighted round-robin balancer over bs.
func NewWeighted(bs []*upstream.Backend) upstream.Balancer {
	return &Weighted{bs: bs, cur: make([]int, len(bs))}
}

func (w *Weighted) Pick(c upstream.Choice) (*upstream.Backend, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	best, total := -1, 0
	for i, b := range w.bs {
		if !c.Usable(b) {
			continue
		}
		wt := weight(b)
		w.cur[i] += wt
		total += wt
		if best == -1 || w.cur[i] > w.cur[best] {
			best = i
		}
	}
	if best == -1 {
		return nil, upstream.ErrNoBackend
	}
	w.cur[best] -= total
	return w.bs[best], nil
}
