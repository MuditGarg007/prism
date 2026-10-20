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

// Observer records a value, such as a latency in seconds. A Prometheus
// histogram is one.
type Observer interface {
	Observe(float64)
}

// Backend is one server in a pool, as one Runtime sees it. A reload builds
// new Backends, but a backend whose URL is still in its upstream keeps its
// state, and its transport and breaker while their settings are unchanged
// (see NewPool).
type Backend struct {
	URL       *url.URL
	Weight    int
	Transport *http.Transport
	// Latency, if set, records how long each attempt that got a response
	// took to receive its headers. It is set before the pool serves.
	Latency Observer

	state *state

	breaker *resilience.Breaker // nil when the upstream has no circuit_breaker
	outlier *resilience.Outlier // shared by the pool; nil without outlier
	index   int                 // this backend's member number in outlier
	log     *slog.Logger
	tc      transportConfig // what Transport was built from
}

// state is what requests and health checks change on a backend. It is
// shared by every Runtime's Backend for the same upstream and URL, so a
// reload keeps least_conn's view of the load, and requests still running on
// the old Runtime release into the same count.
type state struct {
	inflight  atomic.Int64
	unhealthy atomic.Bool // inverted so a new state is available
}

// NewBackend returns an available backend with no transport, breaker or
// outlier detection, for tests and balancers that need only a weight.
func NewBackend(target *url.URL, weight int) *Backend {
	return &Backend{URL: target, Weight: weight, state: &state{}}
}

// Inflight is the number of requests currently being forwarded to b.
func (b *Backend) Inflight() int64 { return b.state.inflight.Load() }

// Acquire marks a request as started on b. Every Acquire needs a Release.
func (b *Backend) Acquire() { b.state.inflight.Add(1) }

// Release marks a request started with Acquire as finished.
func (b *Backend) Release() { b.state.inflight.Add(-1) }

// Available reports whether b may receive requests: it is healthy, not
// ejected, and its circuit breaker is not open. Every check is an atomic
// load, because balancers call this on every pick.
func (b *Backend) Available() bool {
	return !b.state.unhealthy.Load() &&
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
func (b *Backend) SetHealthy(ok bool) { b.state.unhealthy.Store(!ok) }

// Healthy reports whether health checking last found b healthy. A backend
// without health checks is always healthy.
func (b *Backend) Healthy() bool { return !b.state.unhealthy.Load() }

// Ejected reports whether outlier detection has ejected b for now.
func (b *Backend) Ejected() bool { return b.outlier != nil && b.outlier.Ejected(b.index) }

func (b *Backend) String() string { return b.URL.Redacted() }

// Pool is a named group of backends with the balancer that chooses between
// them.
type Pool struct {
	Name     string
	Backends []*Backend
	balancer Balancer

	cfg     *config.Upstream // what the pool was built from, for the next reload
	outlier *resilience.Outlier
}

// NewPool builds the backends of u, a transport for each one, and a balancer
// over them. A transport per backend, not per pool, lets each backend have
// its own TLS settings. State changes of backends, such as a breaker
// opening, are logged to log.
//
// prev is the pool of the same name in the Runtime being replaced, or nil.
// A backend whose URL prev also has keeps its inflight count and health,
// its transport and pooled connections when the transport settings are
// equal, and its breaker when the circuit_breaker config is equal. Outlier
// detection is kept when its config and the whole backend list are equal,
// since it tracks backends by index. Commit must be called before the pool
// serves.
func NewPool(name string, u *config.Upstream, newBal NewBalancer, log *slog.Logger, prev *Pool) (*Pool, error) {
	p := &Pool{Name: name, cfg: u}
	old := map[string]*Backend{}
	if prev != nil {
		for i, b := range prev.Backends {
			old[prev.cfg.Backends[i].URL] = b
		}
	}
	if o := u.Outlier; o != nil {
		if prev != nil && prev.outlier != nil && *prev.cfg.Outlier == *o && sameURLs(prev.cfg.Backends, u.Backends) {
			p.outlier = prev.outlier
		} else {
			p.outlier = resilience.NewOutlier(len(u.Backends), resilience.OutlierConfig{
				ConsecutiveFailures: o.ConsecutiveFailures,
				BaseEjection:        o.BaseEjection,
				MaxEjection:         o.MaxEjection,
				MaxEjectionPercent:  o.MaxEjectionPercent,
			})
		}
	}
	for i, bc := range u.Backends {
		b, err := newBackend(u, bc, old[bc.URL])
		if err != nil {
			// Transports carried over from prev are still serving.
			keep := map[*http.Transport]bool{}
			if prev != nil {
				prev.AddTransports(keep)
			}
			p.CloseExcept(keep)
			return nil, fmt.Errorf("upstream %s: backend %d: %w", name, i, err)
		}
		b.log = log.With("upstream", name, "backend", b.String())
		b.outlier, b.index = p.outlier, i
		if cb := u.CircuitBreaker; cb != nil {
			if ob := old[bc.URL]; ob != nil && ob.breaker != nil && *prev.cfg.CircuitBreaker == *cb {
				b.breaker = ob.breaker
			} else {
				b.breaker = resilience.NewBreaker(resilience.BreakerConfig{
					FailureRatio:   cb.FailureRatio,
					MinRequests:    cb.MinRequests,
					OpenFor:        cb.OpenFor,
					Window:         cb.Window,
					HalfOpenProbes: cb.HalfOpenProbes,
				}, logBreaker(b.log))
			}
		}
		p.Backends = append(p.Backends, b)
	}
	p.balancer = newBal(p.Backends)
	return p, nil
}

// Commit makes the changes to state shared with the previous pool that
// NewPool leaves out, so that a reload that fails after NewPool never
// touches the pool still serving. Without health checks, every backend is
// marked healthy, or one that the previous config's checks marked down
// would stay down for good.
func (p *Pool) Commit() {
	if p.cfg.Health == nil {
		for _, b := range p.Backends {
			b.SetHealthy(true)
		}
	}
}

func sameURLs(a, b []config.Backend) bool {
	return slices.EqualFunc(a, b, func(x, y config.Backend) bool { return x.URL == y.URL })
}

// logBreaker logs a breaker's state changes. It holds only the logger, not
// the Backend, because a carried-over breaker outlives the Backend that
// built it.
func logBreaker(log *slog.Logger) func(from, to resilience.BreakerState) {
	return func(from, to resilience.BreakerState) {
		level := slog.LevelInfo
		if to == resilience.BreakerOpen {
			level = slog.LevelWarn
		}
		log.Log(context.Background(), level, "circuit breaker state changed", "from", from, "to", to)
	}
}

// newBackend builds the backend for bc, keeping old's state, and old's
// transport if it was built from the same settings.
func newBackend(u *config.Upstream, bc config.Backend, old *Backend) (*Backend, error) {
	target, err := url.Parse(bc.URL)
	if err != nil {
		return nil, err
	}
	tc, err := newTransportConfig(u, target, bc.TLS)
	if err != nil {
		return nil, err
	}
	b := NewBackend(target, bc.Weight)
	b.tc = tc
	if old != nil {
		b.state = old.state
		if old.tc == tc {
			b.Transport = old.Transport
			return b, nil
		}
	}
	if b.Transport, err = newTransport(tc); err != nil {
		return nil, err
	}
	return b, nil
}

// Pick chooses a backend for one request.
func (p *Pool) Pick(c Choice) (*Backend, error) {
	return p.balancer.Pick(c)
}

// Close closes idle connections to every backend. Requests in flight keep
// their connections.
func (p *Pool) Close() { p.CloseExcept(nil) }

// CloseExcept closes idle connections on every backend transport not in
// keep, so a reload can retire the old pools without closing connections
// that the new pools carried over.
func (p *Pool) CloseExcept(keep map[*http.Transport]bool) {
	for _, b := range p.Backends {
		if !keep[b.Transport] {
			b.Transport.CloseIdleConnections()
		}
	}
}

// AddTransports adds the transports of p's backends to set.
func (p *Pool) AddTransports(set map[*http.Transport]bool) {
	for _, b := range p.Backends {
		set[b.Transport] = true
	}
}
