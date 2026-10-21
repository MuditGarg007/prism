package config

import (
	"strings"
	"time"
)

const (
	defaultConnectTimeout        = 5 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultIdleConnTimeout       = 90 * time.Second
	defaultMaxIdleConnsPerHost   = 64
	defaultReadHeaderTimeout     = 10 * time.Second
	defaultListenerIdleTimeout   = 120 * time.Second
	defaultHealthInterval        = 10 * time.Second
	defaultHealthTimeout         = 2 * time.Second
	defaultUnhealthyAfter        = 3
	defaultHealthyAfter          = 2
	defaultFailureRatio          = 0.5
	defaultMinRequests           = 20
	defaultOpenFor               = 30 * time.Second
	defaultBreakerWindow         = 10 * time.Second
	defaultHalfOpenProbes        = 3
	defaultConsecutiveFailures   = 5
	defaultBaseEjection          = 30 * time.Second
	defaultMaxEjection           = 300 * time.Second
	defaultMaxEjectionPercent    = 50
	defaultQueueTimeout          = time.Second
	defaultBudgetRatio           = 0.2
	defaultBudgetMinPerSec       = 10
	defaultBackoffBase           = 25 * time.Millisecond
	defaultBackoffMax            = 250 * time.Millisecond
	defaultShutdownTimeout       = 15 * time.Second
	defaultMaxCacheObject        = 1 << 20
	defaultCacheMaxSize          = 64 << 20
	defaultCacheLockTimeout      = 5 * time.Second
	defaultRealm                 = "prism"
	defaultAPIKeyHeader          = "X-API-Key"
	defaultShadowPercent         = 100
	defaultShadowMaxBody         = 64 << 10
	defaultShadowConcurrency     = 16
	defaultShadowTimeout         = 5 * time.Second
	defaultWebSocketIdleTimeout  = 5 * time.Minute
)

// applyDefaults fills zero values only, so invalid input such as a negative
// weight is left for validate to report.
func (c *Config) applyDefaults() {
	setDefault(&c.Shutdown.Timeout, defaultShutdownTimeout)
	setDefault(&c.Cache.MaxSize, defaultCacheMaxSize)
	for i := range c.Listeners {
		t := &c.Listeners[i].Timeouts
		setDefault(&t.ReadHeader, defaultReadHeaderTimeout)
		setDefault(&t.Idle, defaultListenerIdleTimeout)
	}
	for _, u := range c.Upstreams {
		if u == nil {
			continue
		}
		setDefault(&u.Balancer, BalancerRoundRobin)
		for i := range u.Backends {
			setDefault(&u.Backends[i].Weight, 1)
		}
		setDefault(&u.Timeouts.Connect, defaultConnectTimeout)
		setDefault(&u.Timeouts.ResponseHeader, defaultResponseHeaderTimeout)
		setDefault(&u.Timeouts.IdleConn, defaultIdleConnTimeout)
		setDefault(&u.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
		if h := u.Health; h != nil {
			setDefault(&h.Interval, defaultHealthInterval)
			setDefault(&h.Timeout, defaultHealthTimeout)
			setDefault(&h.UnhealthyAfter, defaultUnhealthyAfter)
			setDefault(&h.HealthyAfter, defaultHealthyAfter)
		}
		if cb := u.CircuitBreaker; cb != nil {
			setDefault(&cb.FailureRatio, defaultFailureRatio)
			setDefault(&cb.MinRequests, defaultMinRequests)
			setDefault(&cb.OpenFor, defaultOpenFor)
			setDefault(&cb.Window, defaultBreakerWindow)
			setDefault(&cb.HalfOpenProbes, defaultHalfOpenProbes)
		}
		if o := u.Outlier; o != nil {
			setDefault(&o.ConsecutiveFailures, defaultConsecutiveFailures)
			setDefault(&o.BaseEjection, defaultBaseEjection)
			setDefault(&o.MaxEjection, defaultMaxEjection)
			setDefault(&o.MaxEjectionPercent, defaultMaxEjectionPercent)
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		r.Match.Host = strings.ToLower(r.Match.Host)
		setDefault(&r.Match.PathPrefix, "/")
		if r.QueueSize > 0 {
			setDefault(&r.QueueTimeout, defaultQueueTimeout)
		}
		if rl := r.RateLimit; rl != nil {
			setDefault(&rl.Burst, rl.PerIP.Count)
		}
		if rt := r.Retry; rt != nil {
			if len(rt.On) == 0 {
				rt.On = []string{"connect_error"}
			}
			setDefault(&rt.BudgetRatio, defaultBudgetRatio)
			setDefault(&rt.BudgetMinPerSec, defaultBudgetMinPerSec)
			setDefault(&rt.Backoff.Base, defaultBackoffBase)
			setDefault(&rt.Backoff.Max, defaultBackoffMax)
		}
		if ca := r.Cache; ca != nil {
			setDefault(&ca.MaxObject, defaultMaxCacheObject)
			setDefault(&ca.LockTimeout, defaultCacheLockTimeout)
		}
		if a := r.Auth; a != nil {
			if b := a.Basic; b != nil {
				setDefault(&b.Realm, defaultRealm)
			}
			if k := a.APIKeys; k != nil {
				setDefault(&k.Header, defaultAPIKeyHeader)
			}
		}
		if sh := r.Shadow; sh != nil {
			setDefault(&sh.Percent, defaultShadowPercent)
			setDefault(&sh.MaxBody, defaultShadowMaxBody)
			setDefault(&sh.MaxConcurrency, defaultShadowConcurrency)
			setDefault(&sh.Timeout, defaultShadowTimeout)
		}
		if ws := r.WebSocket; ws != nil {
			setDefault(&ws.IdleTimeout, defaultWebSocketIdleTimeout)
		}
	}
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}
