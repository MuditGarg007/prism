// Package upstream holds the backends that Prism forwards to, grouped into
// pools, along with the HTTP transport used to reach each backend.
package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/config"
)

// ErrNoBackend is returned by a Balancer when every backend is unavailable.
var ErrNoBackend = errors.New("no available backend")

// Balancer picks the backend for a request.
type Balancer interface {
	// Pick must skip unavailable backends and return ErrNoBackend if none
	// is left. It is called concurrently.
	Pick(r *http.Request, bs []*Backend) (*Backend, error)
}

// Backend is one server in a pool.
type Backend struct {
	URL       *url.URL
	Weight    int
	Transport *http.Transport

	inflight  atomic.Int64
	unhealthy atomic.Bool // inverted so a zero Backend is available
}

// Inflight is the number of requests currently being forwarded to b.
func (b *Backend) Inflight() int64 { return b.inflight.Load() }

// Acquire marks a request as started on b. Every Acquire needs a Release.
func (b *Backend) Acquire() { b.inflight.Add(1) }

// Release marks a request started with Acquire as finished.
func (b *Backend) Release() { b.inflight.Add(-1) }

// Available reports whether b may receive requests. M3 adds the circuit
// breaker and outlier ejection to this check.
func (b *Backend) Available() bool { return !b.unhealthy.Load() }

// SetHealthy records the result of health checking b.
func (b *Backend) SetHealthy(ok bool) { b.unhealthy.Store(!ok) }

func (b *Backend) String() string { return b.URL.Redacted() }

// Pool is a named group of backends with the balancer that chooses between
// them.
type Pool struct {
	Name     string
	Backends []*Backend
	balancer Balancer
}

// NewPool builds the backends of u and a transport for each one. A
// transport per backend, not per pool, lets each backend have its own TLS
// settings.
func NewPool(name string, u *config.Upstream, bal Balancer) (*Pool, error) {
	p := &Pool{Name: name, balancer: bal}
	for i, bc := range u.Backends {
		b, err := newBackend(u, bc)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("upstream %s: backend %d: %w", name, i, err)
		}
		p.Backends = append(p.Backends, b)
	}
	return p, nil
}

func newBackend(u *config.Upstream, bc config.Backend) (*Backend, error) {
	target, err := url.Parse(bc.URL)
	if err != nil {
		return nil, err
	}
	tr, err := newTransport(u, target, bc.TLS)
	if err != nil {
		return nil, err
	}
	return &Backend{URL: target, Weight: bc.Weight, Transport: tr}, nil
}

// Pick chooses the backend for r.
func (p *Pool) Pick(r *http.Request) (*Backend, error) {
	return p.balancer.Pick(r, p.Backends)
}

// Close closes idle connections to every backend. Requests in flight keep
// their connections.
func (p *Pool) Close() {
	for _, b := range p.Backends {
		b.Transport.CloseIdleConnections()
	}
}
