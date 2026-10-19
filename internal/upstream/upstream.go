// Package upstream holds the backends that Prism forwards to, grouped into
// pools, along with the HTTP transport used to reach each backend.
package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/config"
)

// ErrNoBackend is returned by a Balancer when every backend is unavailable.
var ErrNoBackend = errors.New("no available backend")

// Balancer picks the backend for a request from the backends it was built
// with. Building it with its backends lets a balancer keep per-backend state,
// such as smooth weighted round robin's current weights or a hash ring.
type Balancer interface {
	// Pick must skip every backend that c.Usable rejects and return
	// ErrNoBackend if none is left. It is called concurrently.
	Pick(c Choice) (*Backend, error)
}

// NewBalancer builds a Balancer over a pool's backends.
type NewBalancer func(bs []*Backend) Balancer

// Choice is what a Balancer knows about the request it picks for.
type Choice struct {
	// ClientIP is the client address worked out from trusted proxies, never
	// the raw X-Forwarded-For. It is invalid for non-IP peers.
	ClientIP netip.Addr
	// Tried holds the backends this request has already failed on, so a
	// retry goes to a different one.
	Tried []*Backend
}

// Usable reports whether b may be picked: it is available and has not been
// tried yet.
func (c Choice) Usable(b *Backend) bool {
	return b.Available() && !slices.Contains(c.Tried, b)
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

// NewPool builds the backends of u, a transport for each one, and a balancer
// over them. A transport per backend, not per pool, lets each backend have
// its own TLS settings.
func NewPool(name string, u *config.Upstream, newBal NewBalancer) (*Pool, error) {
	p := &Pool{Name: name}
	for i, bc := range u.Backends {
		b, err := newBackend(u, bc)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("upstream %s: backend %d: %w", name, i, err)
		}
		p.Backends = append(p.Backends, b)
	}
	p.balancer = newBal(p.Backends)
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

// Pick chooses a backend for one request.
func (p *Pool) Pick(c Choice) (*Backend, error) {
	return p.balancer.Pick(c)
}

// Close closes idle connections to every backend. Requests in flight keep
// their connections.
func (p *Pool) Close() {
	for _, b := range p.Backends {
		b.Transport.CloseIdleConnections()
	}
}
