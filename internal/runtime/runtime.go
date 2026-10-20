// Package runtime turns a validated config into the objects that serve
// requests, and holds the current set of them so that a reload can replace
// everything in one atomic step.
package runtime

import (
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/balancer"
	"github.com/MuditGarg007/prism/internal/cache"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/health"
	"github.com/MuditGarg007/prism/internal/limits"
	"github.com/MuditGarg007/prism/internal/proxy"
	"github.com/MuditGarg007/prism/internal/ratelimit"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/router"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// Runtime is everything built from one config. It is never changed after
// Build; a reload builds a new one.
type Runtime struct {
	Config  *config.Config
	Handler *proxy.Handler
	Pools   map[string]*upstream.Pool
	// Cache is shared by every route that caches, or nil when none does.
	Cache *Cache

	checkers []*health.Checker // started by Start
}

// Cache is the response cache that every caching route shares. The admin
// API reads and purges it.
type Cache struct {
	Store   *cache.Store
	Flight  *cache.Flight
	Stats   *cache.Stats
	MaxSize int64 // the Store's byte budget, cache.max_size
}

func caches(r config.Route) bool { return r.Cache != nil && r.Cache.Enabled }

// Build creates the pools, routes and handler for cfg, which must already be
// validated. It fails on problems that validation cannot see, such as a CA
// file that holds no certificates.
func Build(cfg *config.Config, log *slog.Logger) (*Runtime, error) {
	rt := &Runtime{Config: cfg, Pools: make(map[string]*upstream.Pool, len(cfg.Upstreams))}
	if a := cfg.Admin; a != nil && !loopback(a.Listen) {
		log.Warn("admin API listens beyond loopback and has no authentication", "addr", a.Listen)
	}
	// One store for the whole process, built only when some route uses it.
	if slices.ContainsFunc(cfg.Routes, caches) {
		size := int64(cfg.Cache.MaxSize)
		rt.Cache = &Cache{
			Store:   cache.NewStore(size),
			Flight:  cache.NewFlight(),
			Stats:   &cache.Stats{},
			MaxSize: size,
		}
	}

	// Sorted, so warnings and errors come out in the same order every time.
	for _, name := range slices.Sorted(maps.Keys(cfg.Upstreams)) {
		u := cfg.Upstreams[name]
		pool, err := upstream.NewPool(name, u, balancer.New(u.Balancer), log)
		if err != nil {
			rt.Close()
			return nil, err
		}
		rt.Pools[name] = pool
		if h := u.Health; h != nil {
			rt.checkers = append(rt.checkers, health.New(pool, health.Config{
				Path:           h.Path,
				Interval:       h.Interval,
				Timeout:        h.Timeout,
				UnhealthyAfter: h.UnhealthyAfter,
				HealthyAfter:   h.HealthyAfter,
			}, log))
		}
	}

	entries := make([]router.Entry[*proxy.Route], 0, len(cfg.Routes))
	for i, r := range cfg.Routes {
		pool, ok := rt.Pools[r.Upstream]
		if !ok {
			rt.Close()
			return nil, fmt.Errorf("routes[%d]: unknown upstream %q", i, r.Upstream)
		}
		h := r.Headers
		var limiter *ratelimit.Limiter
		if rl := r.RateLimit; rl != nil {
			limiter = ratelimit.New(rl.PerIP.Count, rl.PerIP.Per, rl.Burst)
		}
		var retry *resilience.Retry
		if rc := r.Retry; rc != nil && rc.Attempts > 0 {
			var err error
			if retry, err = resilience.NewRetry(rc.Attempts, rc.On); err != nil {
				rt.Close()
				return nil, fmt.Errorf("routes[%d].retry: %w", i, err)
			}
			retry.Backoff = resilience.Backoff{Base: rc.Backoff.Base, Max: rc.Backoff.Max}
			retry.Budget = resilience.NewBudget(rc.BudgetRatio, rc.BudgetMinPerSec)
		}
		var pc *proxy.Cache
		if caches(r) {
			c := r.Cache
			pc = &proxy.Cache{
				Store:        rt.Cache.Store,
				Flight:       rt.Cache.Flight,
				Stats:        rt.Cache.Stats,
				MaxObject:    int64(c.MaxObject),
				LockTimeout:  c.LockTimeout,
				BypassHeader: http.CanonicalHeaderKey(c.BypassHeader),
				StaleIfError: c.StaleIfError,
			}
		}
		var sem *limits.Semaphore
		if r.MaxConcurrency > 0 {
			sem = limits.NewSemaphore(r.MaxConcurrency, r.QueueSize)
		}
		entries = append(entries, router.Entry[*proxy.Route]{
			Host:       r.Match.Host,
			PathPrefix: r.Match.PathPrefix,
			Value: &proxy.Route{
				Name:         r.Match.Host + r.Match.PathPrefix,
				Pool:         pool,
				PathPrefix:   r.Match.PathPrefix,
				StripPrefix:  r.StripPrefix,
				PreserveHost: r.PreserveHost,
				Request:      proxy.NewHeaderOps(h.RequestSet, h.RequestRemove),
				Response:     proxy.NewHeaderOps(h.ResponseSet, h.ResponseRemove),
				RateLimit:    limiter,
				MaxBody:      int64(r.MaxBody),
				Concurrency:  sem,
				QueueTimeout: r.QueueTimeout,
				Retry:        retry,
				Cache:        pc,
			},
		})
	}
	table, err := router.Compile(entries)
	if err != nil {
		rt.Close()
		return nil, err
	}

	trusted := make([]netip.Prefix, len(cfg.TrustedProxies))
	for i, p := range cfg.TrustedProxies {
		trusted[i] = p.Prefix
	}
	rt.Handler = proxy.NewHandler(table, proxy.NewForwarding(trusted, cfg.ForwardedHeader), log)
	return rt, nil
}

// loopback reports whether addr only accepts connections from this host.
// An empty host listens on every interface.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Start starts the background work of rt, which is active health checking.
// Build never starts it, so -validate sends nothing to backends. It must be
// called at most once.
func (rt *Runtime) Start() {
	for _, c := range rt.checkers {
		c.Start()
	}
}

// Close stops health checking and closes the idle backend connections of
// every pool. Requests still using rt keep their connections.
func (rt *Runtime) Close() {
	for _, c := range rt.checkers {
		c.Stop()
	}
	for _, p := range rt.Pools {
		p.Close()
	}
}

// Current serves requests with whichever Runtime is current. Each request
// loads it once, so it is served entirely by one config even if a reload
// swaps in another halfway through.
type Current struct {
	p atomic.Pointer[Runtime]
}

// NewCurrent returns a Current that starts with rt.
func NewCurrent(rt *Runtime) *Current {
	c := &Current{}
	c.p.Store(rt)
	return c
}

// Load returns the current Runtime.
func (c *Current) Load() *Runtime { return c.p.Load() }

// Swap makes rt current and returns the previous Runtime. Requests already
// running keep using the old one, so the caller must let them drain before
// closing it.
func (c *Current) Swap(rt *Runtime) *Runtime { return c.p.Swap(rt) }

func (c *Current) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.p.Load().Handler.ServeHTTP(w, r)
}
