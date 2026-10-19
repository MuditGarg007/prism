package config

import (
	"strings"
	"time"
)

const (
	defaultConnectTimeout        = 5 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultHealthInterval        = 10 * time.Second
	defaultHealthTimeout         = 2 * time.Second
	defaultUnhealthyAfter        = 3
	defaultHealthyAfter          = 2
	defaultFailureRatio          = 0.5
	defaultMinRequests           = 20
	defaultOpenFor               = 30 * time.Second
	defaultBudgetRatio           = 0.2
	defaultMaxCacheObject        = 1 << 20
)

// applyDefaults fills zero values only, so invalid input such as a negative
// weight is left for validate to report.
func (c *Config) applyDefaults() {
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
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		r.Match.Host = strings.ToLower(r.Match.Host)
		setDefault(&r.Match.PathPrefix, "/")
		if rl := r.RateLimit; rl != nil {
			setDefault(&rl.Burst, rl.PerIP.Count)
		}
		if rt := r.Retry; rt != nil {
			if len(rt.On) == 0 {
				rt.On = []string{"connect_error"}
			}
			setDefault(&rt.BudgetRatio, defaultBudgetRatio)
		}
		if ca := r.Cache; ca != nil {
			setDefault(&ca.MaxObject, defaultMaxCacheObject)
		}
	}
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}
