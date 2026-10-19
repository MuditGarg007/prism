// Package balancer implements the strategies that pick a backend from a
// pool.
package balancer

import (
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// New returns the constructor for a config balancer name. Config validation
// has already rejected unknown names.
func New(kind string) upstream.NewBalancer {
	switch kind {
	case config.BalancerWeighted:
		return NewWeighted
	case config.BalancerLeastConn:
		return NewLeastConn
	case config.BalancerIPHash:
		return NewIPHash
	default:
		return NewRoundRobin
	}
}

// weight is b's configured weight. Config requires at least 1; a zero
// Backend counts as 1.
func weight(b *upstream.Backend) int { return max(b.Weight, 1) }

// RoundRobin cycles through backends in order with a shared atomic counter.
// A backend that cannot be used is skipped, so its turn goes to the next one.
type RoundRobin struct {
	bs   []*upstream.Backend
	next atomic.Uint64
}

// NewRoundRobin returns a round-robin balancer over bs.
func NewRoundRobin(bs []*upstream.Backend) upstream.Balancer {
	return &RoundRobin{bs: bs}
}

func (rr *RoundRobin) Pick(c upstream.Choice) (*upstream.Backend, error) {
	n := uint64(len(rr.bs))
	if n == 0 {
		return nil, upstream.ErrNoBackend
	}
	start := rr.next.Add(1) - 1
	for i := range n {
		if b := rr.bs[(start+i)%n]; c.Usable(b) {
			return b, nil
		}
	}
	return nil, upstream.ErrNoBackend
}
