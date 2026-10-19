// Package balancer implements the strategies that pick a backend from a
// pool.
package balancer

import (
	"net/http"
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// New returns the balancer for a config balancer name.
//
// TODO(M2): weighted, least_conn and ip_hash fall back to round robin until
// they are implemented; see Implemented.
func New(kind string) upstream.Balancer {
	return &RoundRobin{}
}

// Implemented reports whether kind has its own implementation yet, rather
// than the round-robin fallback.
func Implemented(kind string) bool {
	return kind == config.BalancerRoundRobin
}

// RoundRobin cycles through backends in order with a shared atomic counter.
// An unavailable backend is skipped, so its turn goes to the next one.
type RoundRobin struct {
	next atomic.Uint64
}

func (rr *RoundRobin) Pick(_ *http.Request, bs []*upstream.Backend) (*upstream.Backend, error) {
	n := uint64(len(bs))
	if n == 0 {
		return nil, upstream.ErrNoBackend
	}
	start := rr.next.Add(1) - 1
	for i := range n {
		if b := bs[(start+i)%n]; b.Available() {
			return b, nil
		}
	}
	return nil, upstream.ErrNoBackend
}
