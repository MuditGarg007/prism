// Package upstream holds the backends that Prism forwards to, grouped into
// pools, along with the HTTP transport used to reach each backend.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/resilience"
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

	breaker *resilience.Breaker // nil when the upstream has no circuit_breaker
	outlier *resilience.Outlier // shared by the pool; nil without outlier
	index   int                 // this backend's member number in outlier
	log     *slog.Logger
}

// Inflight is the number of requests currently being forwarded to b.
func (b *Backend) Inflight() int64 { return b.inflight.Load() }

// Acquire marks a request as started on b. Every Acquire needs a Release.
func (b *Backend) Acquire() { b.inflight.Add(1) }

// Release marks a request started with Acquire as finished.
func (b *Backend) Release() { b.inflight.Add(-1) }

// Available reports whether b may receive requests: it is healthy, not
// ejected, and its circuit breaker is not open. Every check is an atomic
// load, because balancers call this on every pick.
func (b *Backend) Available() bool {
	return !b.unhealthy.Load() &&
		(b.breaker == nil || b.breaker.Ready()) &&
		(b.outlier == nil || !b.outlier.Ejected(b.index))
}

// Admission is a request's permission to use a backend, from Admit. It goes
// back to Report with the request's outcome.
type Admission struct{ ticket resilience.Ticket }

// Admit asks b's circuit breaker to let a request through. Balancers only
// skip backends whose breaker is open; a half-open breaker refuses here once
// its probe slots are taken. Every admitted request must be reported.
func (b *Backend) Admit() (Admission, bool) {
	if b.breaker == nil {
		return Admission{}, true
	}
	t, ok := b.breaker.Allow()
	return Admission{t}, ok
}

// Report records how an admitted request went, for the circuit breaker and
// outlier detection.
func (b *Backend) Report(a Admission, out resilience.Outcome) {
	if b.breaker != nil {
		b.breaker.Report(a.ticket, out)
	}
	if b.outlier != nil {
		if d := b.outlier.Report(b.index, out); d > 0 {
			b.log.Warn("backend ejected after consecutive failures", "for", d)
		}
	}
}

// Overturn records that a request reported as a success failed after all,
// because the response body broke off.
func (b *Backend) Overturn(a Admission) {
	if b.breaker != nil {
		b.breaker.Overturn(a.ticket)
	}
	if b.outlier != nil {
		if d := b.outlier.Report(b.index, resilience.Failure); d > 0 {
			b.log.Warn("backend ejected after consecutive failures", "for", d)
		}
	}
}

// Breaker returns b's circuit breaker, or nil if it has none.
func (b *Backend) Breaker() *resilience.Breaker { return b.breaker }

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
// its own TLS settings. State changes of backends, such as a breaker
// opening, are logged to log.
func NewPool(name string, u *config.Upstream, newBal NewBalancer, log *slog.Logger) (*Pool, error) {
	p := &Pool{Name: name}
	var outlier *resilience.Outlier
	if o := u.Outlier; o != nil {
		outlier = resilience.NewOutlier(len(u.Backends), resilience.OutlierConfig{
			ConsecutiveFailures: o.ConsecutiveFailures,
			BaseEjection:        o.BaseEjection,
			MaxEjection:         o.MaxEjection,
			MaxEjectionPercent:  o.MaxEjectionPercent,
		})
	}
	for i, bc := range u.Backends {
		b, err := newBackend(u, bc)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("upstream %s: backend %d: %w", name, i, err)
		}
		b.log = log.With("upstream", name, "backend", b.String())
		b.outlier, b.index = outlier, i
		if cb := u.CircuitBreaker; cb != nil {
			b.breaker = resilience.NewBreaker(resilience.BreakerConfig{
				FailureRatio:   cb.FailureRatio,
				MinRequests:    cb.MinRequests,
				OpenFor:        cb.OpenFor,
				Window:         cb.Window,
				HalfOpenProbes: cb.HalfOpenProbes,
			}, b.logBreaker)
		}
		p.Backends = append(p.Backends, b)
	}
	p.balancer = newBal(p.Backends)
	return p, nil
}

func (b *Backend) logBreaker(from, to resilience.BreakerState) {
	level := slog.LevelInfo
	if to == resilience.BreakerOpen {
		level = slog.LevelWarn
	}
	b.log.Log(context.Background(), level, "circuit breaker state changed", "from", from, "to", to)
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
