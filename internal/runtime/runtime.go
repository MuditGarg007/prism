// Package runtime turns a validated config into the objects that serve
// requests, and holds the current set of them so that a reload can replace
// everything in one atomic step.
package runtime

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/balancer"
	"github.com/MuditGarg007/prism/internal/cache"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/health"
	"github.com/MuditGarg007/prism/internal/limits"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/proxy"
	"github.com/MuditGarg007/prism/internal/ratelimit"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/router"
	"github.com/MuditGarg007/prism/internal/security"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// Runtime is everything built from one config. It is never changed after
// Build; a reload builds a new one.
type Runtime struct {
	Config  *config.Config
	Handler *proxy.Handler
	Pools   map[string]*upstream.Pool
	// Routes are the routes Handler matches, in config order.
	Routes []*proxy.Route
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

// Options are what Build needs besides the config.
type Options struct {
	// Metrics, when set, gives routes and backends their metric handles.
	// It is shared by every Runtime of the process.
	Metrics *observability.Metrics
	// Prev is the Runtime a reload replaces, or nil. Build carries its
	// state over where the config allows (see upstream.NewPool), and
	// shares, never closes, what it carries.
	Prev *Runtime
}

// Build creates the pools, routes and handler for cfg, which must already be
// validated. It fails on problems that validation cannot see, such as a CA
// file that holds no certificates.
func Build(cfg *config.Config, log *slog.Logger, opts Options) (_ *Runtime, err error) {
	m, prev := opts.Metrics, opts.Prev
	rt := &Runtime{Config: cfg, Pools: make(map[string]*upstream.Pool, len(cfg.Upstreams))}
	defer func() {
		if err != nil {
			rt.Retire(prev) // prev is still serving with what it shares
		}
	}()
	if a := cfg.Admin; a != nil && !loopback(a.Listen) {
		log.Warn("admin API listens beyond loopback and has no authentication", "addr", a.Listen)
	}
	// One store for the whole process, built only when some route uses it.
	// A reload keeps the entries, the fetches in progress and the counters.
	if slices.ContainsFunc(cfg.Routes, caches) {
		size := int64(cfg.Cache.MaxSize)
		if prev != nil && prev.Cache != nil {
			c := *prev.Cache
			c.MaxSize = size // the Store is resized once Build has succeeded
			rt.Cache = &c
		} else {
			rt.Cache = &Cache{
				Store:   cache.NewStore(size),
				Flight:  cache.NewFlight(),
				Stats:   &cache.Stats{},
				MaxSize: size,
			}
		}
	}

	// Sorted, so warnings and errors come out in the same order every time.
	for _, name := range slices.Sorted(maps.Keys(cfg.Upstreams)) {
		u := cfg.Upstreams[name]
		var prevPool *upstream.Pool
		if prev != nil {
			prevPool = prev.Pools[name]
		}
		pool, err := upstream.NewPool(name, u, balancer.New(u.Balancer), log, prevPool)
		if err != nil {
			return nil, err
		}
		rt.Pools[name] = pool
		if m != nil {
			for _, b := range pool.Backends {
				b.Latency = m.Backend(name, b.String())
			}
		}
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

	// A route keeps its limiter, slots and budget across a reload, matched
	// by name, while their settings are unchanged. Otherwise every reload
	// would reset rate limits and briefly double concurrency caps.
	type prevRoute struct {
		cfg   config.Route
		route *proxy.Route
	}
	prevRoutes := map[string]prevRoute{}
	if prev != nil {
		for i, r := range prev.Routes {
			prevRoutes[r.Name] = prevRoute{prev.Config.Routes[i], r}
		}
	}

	entries := make([]router.Entry[*proxy.Route], 0, len(cfg.Routes))
	for i, r := range cfg.Routes {
		name := r.Match.Host + r.Match.PathPrefix
		old, hasOld := prevRoutes[name]
		if r.Auth != nil && r.Auth.Basic != nil && r.RateLimit == nil {
			// Only the route's rate limit slows password guessing.
			log.Warn("route checks passwords but has no rate_limit, so guessing is limited only by bcrypt", "route", name)
		}
		pool, ok := rt.Pools[r.Upstream]
		if !ok {
			return nil, fmt.Errorf("routes[%d]: unknown upstream %q", i, r.Upstream)
		}
		h := r.Headers
		var limiter *ratelimit.Limiter
		if rl := r.RateLimit; rl != nil {
			if hasOld && old.cfg.RateLimit != nil && *old.cfg.RateLimit == *rl {
				limiter = old.route.RateLimit
			} else {
				limiter = ratelimit.New(rl.PerIP.Count, rl.PerIP.Per, rl.Burst)
			}
		}
		var retry *resilience.Retry
		if rc := r.Retry; rc != nil && rc.Attempts > 0 {
			var err error
			if retry, err = resilience.NewRetry(rc.Attempts, rc.On); err != nil {
				return nil, fmt.Errorf("routes[%d].retry: %w", i, err)
			}
			retry.Backoff = resilience.Backoff{Base: rc.Backoff.Base, Max: rc.Backoff.Max}
			if pr := old.route; hasOld && pr.Retry != nil && sameBudget(old.cfg.Retry, rc) {
				retry.Budget = pr.Retry.Budget
			} else {
				retry.Budget = resilience.NewBudget(rc.BudgetRatio, rc.BudgetMinPerSec)
			}
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
			if hasOld && old.route.Concurrency != nil &&
				old.cfg.MaxConcurrency == r.MaxConcurrency && old.cfg.QueueSize == r.QueueSize {
				sem = old.route.Concurrency
			} else {
				sem = limits.NewSemaphore(r.MaxConcurrency, r.QueueSize)
			}
		}
		var rm *observability.RouteMetrics
		if m != nil {
			rm = m.Route(name, r.Upstream)
		}
		route := &proxy.Route{
			Name:         name,
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
			Metrics:      rm,
		}
		if a := r.Access; a != nil {
			route.Access = ipFilter(a)
		}
		if ws := r.WebSocket; ws != nil {
			route.WebSocketIdle = ws.IdleTimeout
		}
		if ca := r.Canary; ca != nil {
			route.Canary = &proxy.Canary{
				Pool:    rt.Pools[ca.Upstream],
				Buckets: uint32(math.Round(ca.Percent * 100)),
				Cookie:  ca.Cookie,
			}
			if ca.Header != "" {
				route.Canary.Header = http.CanonicalHeaderKey(ca.Header)
			}
			if m != nil {
				route.Canary.Metrics = m.Route(name, ca.Upstream)
			}
		}
		if sh := r.Shadow; sh != nil {
			route.Shadow = &proxy.Shadow{
				Pool:    rt.Pools[sh.Upstream],
				Buckets: uint32(math.Round(sh.Percent * 100)),
				MaxBody: int64(sh.MaxBody),
				Timeout: sh.Timeout,
			}
			// Copies still running on the old runtime keep counting
			// against the cap.
			if hasOld && old.route.Shadow != nil && old.cfg.Shadow.MaxConcurrency == sh.MaxConcurrency {
				route.Shadow.Slots = old.route.Shadow.Slots
			} else {
				route.Shadow.Slots = limits.NewSemaphore(sh.MaxConcurrency, 0)
			}
			if m != nil {
				route.Shadow.Metrics = m.Shadow(name)
			}
		}
		if a := r.Auth; a != nil {
			// Kept while unchanged, with the passwords it has verified.
			if hasOld && old.route.Auth != nil && reflect.DeepEqual(old.cfg.Auth, a) {
				route.Auth = old.route.Auth
			} else {
				route.Auth = authenticator(a)
			}
		}
		rt.Routes = append(rt.Routes, route)
		entries = append(entries, router.Entry[*proxy.Route]{
			Host:       r.Match.Host,
			PathPrefix: r.Match.PathPrefix,
			Value:      route,
		})
	}
	table, err := router.Compile(entries)
	if err != nil {
		return nil, err
	}

	trusted := prefixes(cfg.TrustedProxies)
	var unmatched *observability.RouteMetrics
	if m != nil {
		unmatched = m.Unmatched()
	}
	var access *security.IPFilter
	if a := cfg.Access; a != nil {
		access = ipFilter(a)
	}
	rt.Handler = proxy.NewHandler(table, proxy.NewForwarding(trusted, cfg.ForwardedHeader), access, unmatched, log)
	// Last, so a reload that fails never shrinks the cache or changes the
	// backend health that prev serves with.
	if c := rt.Cache; prev != nil && prev.Cache != nil && c != nil && c.MaxSize != prev.Cache.MaxSize {
		c.Store.Resize(c.MaxSize)
	}
	for _, p := range rt.Pools {
		p.Commit()
	}
	return rt, nil
}

func authenticator(a *config.Auth) *security.Authenticator {
	c := security.AuthConfig{Forward: a.ForwardCredentials, IdentityHeader: a.IdentityHeader}
	if b := a.Basic; b != nil {
		c.Realm, c.Users = b.Realm, b.Users
	}
	if k := a.APIKeys; k != nil {
		c.KeyHeader = k.Header
		c.Keys = make(map[[sha256.Size]byte]string, len(k.Keys))
		for name, s := range k.Keys {
			d, _ := config.ParseKeyDigest(s) // validated
			c.Keys[d] = name
		}
	}
	return security.NewAuthenticator(c)
}

func ipFilter(a *config.Access) *security.IPFilter {
	return security.NewIPFilter(prefixes(a.Allow), prefixes(a.Deny))
}

func prefixes(ps []config.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, len(ps))
	for i, p := range ps {
		out[i] = p.Prefix
	}
	return out
}

func sameBudget(a, b *config.Retry) bool {
	return a != nil && a.BudgetRatio == b.BudgetRatio && a.BudgetMinPerSec == b.BudgetMinPerSec
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
func (rt *Runtime) Close() { rt.Retire(nil) }

// Retire is Close for a Runtime that next replaced: it leaves open the
// idle connections of the transports next carried over. A nil next carried
// nothing.
func (rt *Runtime) Retire(next *Runtime) {
	for _, c := range rt.checkers {
		c.Stop()
	}
	var keep map[*http.Transport]bool
	if next != nil {
		keep = map[*http.Transport]bool{}
		for _, p := range next.Pools {
			p.AddTransports(keep)
		}
	}
	for _, p := range rt.Pools {
		p.CloseExcept(keep)
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
