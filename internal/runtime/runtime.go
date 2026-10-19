// Package runtime turns a validated config into the objects that serve
// requests, and holds the current set of them so that a reload can replace
// everything in one atomic step.
package runtime

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"sync/atomic"

	"github.com/MuditGarg007/prism/internal/balancer"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/proxy"
	"github.com/MuditGarg007/prism/internal/router"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// Runtime is everything built from one config. It is never changed after
// Build; a reload builds a new one.
type Runtime struct {
	Config  *config.Config
	Handler *proxy.Handler
	Pools   map[string]*upstream.Pool
}

// Build creates the pools, routes and handler for cfg, which must already be
// validated. It fails on problems that validation cannot see, such as a CA
// file that holds no certificates.
func Build(cfg *config.Config, log *slog.Logger) (*Runtime, error) {
	rt := &Runtime{Config: cfg, Pools: make(map[string]*upstream.Pool, len(cfg.Upstreams))}

	// Sorted, so warnings and errors come out in the same order every time.
	for _, name := range slices.Sorted(maps.Keys(cfg.Upstreams)) {
		u := cfg.Upstreams[name]
		if !balancer.Implemented(u.Balancer) {
			log.Warn("balancer is not implemented yet, using round_robin",
				"upstream", name, "balancer", u.Balancer)
		}
		warnIgnored(log, "upstreams."+name, map[string]bool{
			"health":          u.Health != nil,
			"circuit_breaker": u.CircuitBreaker != nil,
		})
		pool, err := upstream.NewPool(name, u, balancer.New(u.Balancer))
		if err != nil {
			rt.Close()
			return nil, err
		}
		rt.Pools[name] = pool
	}

	entries := make([]router.Entry[*proxy.Route], 0, len(cfg.Routes))
	for i, r := range cfg.Routes {
		pool, ok := rt.Pools[r.Upstream]
		if !ok {
			rt.Close()
			return nil, fmt.Errorf("routes[%d]: unknown upstream %q", i, r.Upstream)
		}
		warnIgnored(log, fmt.Sprintf("routes[%d]", i), map[string]bool{
			"rate_limit":      r.RateLimit != nil,
			"max_body":        r.MaxBody != 0,
			"max_concurrency": r.MaxConcurrency != 0,
			"retry":           r.Retry != nil,
			"cache":           r.Cache != nil,
		})
		h := r.Headers
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

// warnIgnored warns about each config feature in set that is present but not
// implemented yet, so that a limit such as max_body is never silently
// unenforced.
func warnIgnored(log *slog.Logger, path string, set map[string]bool) {
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if set[name] {
			log.Warn("config option is not implemented yet and is ignored", "option", path+"."+name)
		}
	}
}

// Close closes the idle backend connections of every pool. Requests still
// using rt keep their connections.
func (rt *Runtime) Close() {
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
