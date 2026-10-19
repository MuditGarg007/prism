package config

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ValidationError lists every problem found in a config. Each problem is
// prefixed with the path of the field it refers to.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid config:\n  " + strings.Join(e.Problems, "\n  ")
}

type problems []string

func (p *problems) add(path, format string, args ...any) {
	*p = append(*p, path+": "+fmt.Sprintf(format, args...))
}

func (c *Config) validate() error {
	var p problems
	c.validateListeners(&p)
	c.validateUpstreams(&p)
	c.validateRoutes(&p)
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

func (c *Config) validateListeners(p *problems) {
	if len(c.Listeners) == 0 {
		p.add("listeners", "at least one listener is required")
	}
	seen := map[string]bool{}
	for i, l := range c.Listeners {
		path := fmt.Sprintf("listeners[%d]", i)
		if l.Addr == "" {
			p.add(path+".addr", "is required")
		} else if _, port, err := net.SplitHostPort(l.Addr); err != nil {
			p.add(path+".addr", "invalid address %q: %v", l.Addr, err)
		} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			p.add(path+".addr", "invalid port %q", port)
		}
		if seen[l.Addr] {
			p.add(path+".addr", "duplicate listener address %q", l.Addr)
		}
		seen[l.Addr] = true
		checkPositive(p, path+".timeouts.read_header", l.Timeouts.ReadHeader)
		checkPositive(p, path+".timeouts.idle", l.Timeouts.Idle)

		if l.TLS == nil {
			continue
		}
		if len(l.TLS.Certs) == 0 {
			p.add(path+".tls.certs", "at least one certificate is required")
		}
		for j, cp := range l.TLS.Certs {
			cpath := fmt.Sprintf("%s.tls.certs[%d]", path, j)
			checkFile(p, cpath+".cert", cp.Cert)
			checkFile(p, cpath+".key", cp.Key)
		}
	}
}

var balancers = []string{BalancerRoundRobin, BalancerWeighted, BalancerLeastConn, BalancerIPHash}

func (c *Config) validateUpstreams(p *problems) {
	if len(c.Upstreams) == 0 {
		p.add("upstreams", "at least one upstream is required")
	}
	for _, name := range slices.Sorted(maps.Keys(c.Upstreams)) {
		u := c.Upstreams[name]
		path := "upstreams." + name
		if u == nil {
			p.add(path, "upstream is empty")
			continue
		}
		if !slices.Contains(balancers, u.Balancer) {
			p.add(path+".balancer", "unknown balancer %q (want one of %s)", u.Balancer, strings.Join(balancers, ", "))
		}
		if len(u.Backends) == 0 {
			p.add(path+".backends", "at least one backend is required")
		}
		seen := map[string]bool{}
		for i, b := range u.Backends {
			bpath := fmt.Sprintf("%s.backends[%d]", path, i)
			validateBackend(p, bpath, b)
			if seen[b.URL] {
				p.add(bpath+".url", "duplicate backend %q", b.URL)
			}
			seen[b.URL] = true
		}
		checkPositive(p, path+".timeouts.connect", u.Timeouts.Connect)
		checkPositive(p, path+".timeouts.response_header", u.Timeouts.ResponseHeader)
		checkPositive(p, path+".timeouts.idle_conn", u.Timeouts.IdleConn)
		checkMin(p, path+".max_idle_conns_per_host", u.MaxIdleConnsPerHost, 1)
		if h := u.Health; h != nil {
			validateHealth(p, path+".health", h)
		}
		if cb := u.CircuitBreaker; cb != nil {
			validateBreaker(p, path+".circuit_breaker", cb)
		}
	}
}

func validateBackend(p *problems, path string, b Backend) {
	u, err := url.Parse(b.URL)
	switch {
	case b.URL == "":
		p.add(path+".url", "is required")
	case err != nil:
		p.add(path+".url", "%v", err)
	case u.Scheme != "http" && u.Scheme != "https":
		p.add(path+".url", "scheme must be http or https, got %q", u.Scheme)
	case u.Host == "":
		p.add(path+".url", "missing host in %q", b.URL)
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		p.add(path+".url", "must not have a path, query or fragment: %q", b.URL)
	}
	if b.Weight < 1 {
		p.add(path+".weight", "must be at least 1, got %d", b.Weight)
	}
	if b.TLS != nil {
		if err == nil && u.Scheme == "http" {
			p.add(path+".tls", "set on a plain http backend")
		}
		if b.TLS.CAFile != "" {
			checkFile(p, path+".tls.ca_file", b.TLS.CAFile)
		}
	}
}

func validateHealth(p *problems, path string, h *HealthCheck) {
	if !strings.HasPrefix(h.Path, "/") {
		p.add(path+".path", "must start with \"/\", got %q", h.Path)
	}
	checkPositive(p, path+".interval", h.Interval)
	checkPositive(p, path+".timeout", h.Timeout)
	if h.Interval > 0 && h.Timeout >= h.Interval {
		p.add(path+".timeout", "must be less than interval (%v)", h.Interval)
	}
	checkMin(p, path+".unhealthy_after", h.UnhealthyAfter, 1)
	checkMin(p, path+".healthy_after", h.HealthyAfter, 1)
}

func validateBreaker(p *problems, path string, cb *CircuitBreaker) {
	if cb.FailureRatio <= 0 || cb.FailureRatio > 1 {
		p.add(path+".failure_ratio", "must be in (0, 1], got %v", cb.FailureRatio)
	}
	checkMin(p, path+".min_requests", cb.MinRequests, 1)
	checkPositive(p, path+".open_for", cb.OpenFor)
}

func (c *Config) validateRoutes(p *problems) {
	if len(c.Routes) == 0 {
		p.add("routes", "at least one route is required")
	}
	seen := map[Match]int{}
	for i, r := range c.Routes {
		path := fmt.Sprintf("routes[%d]", i)
		if err := validateHost(r.Match.Host); err != nil {
			p.add(path+".match.host", "%v", err)
		}
		if !strings.HasPrefix(r.Match.PathPrefix, "/") {
			p.add(path+".match.path_prefix", "must start with \"/\", got %q", r.Match.PathPrefix)
		}
		if j, ok := seen[r.Match]; ok {
			p.add(path+".match", "same host and path_prefix as routes[%d]", j)
		} else {
			seen[r.Match] = i
		}

		if r.Upstream == "" {
			p.add(path+".upstream", "is required")
		} else if _, ok := c.Upstreams[r.Upstream]; !ok {
			p.add(path+".upstream", "unknown upstream %q", r.Upstream)
		}

		if rl := r.RateLimit; rl != nil {
			if rl.PerIP.Count == 0 {
				p.add(path+".rate_limit.per_ip", "is required")
			} else {
				checkMin(p, path+".rate_limit.burst", rl.Burst, 1)
			}
		}
		validateHeaders(p, path+".headers", r.Headers)
		checkMin(p, path+".max_concurrency", r.MaxConcurrency, 0)
		if rt := r.Retry; rt != nil {
			validateRetry(p, path+".retry", rt)
		}
	}
}

// validateHost accepts "" (any host), an exact name, or a wildcard such as
// "*.example.com". Hosts are already lowercased by applyDefaults.
func validateHost(h string) error {
	if h == "" {
		return nil
	}
	name := strings.TrimPrefix(h, "*.")
	if strings.Contains(name, "*") {
		return fmt.Errorf("invalid host %q: \"*\" is only allowed as a leading \"*.\"", h)
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("invalid host %q", h)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("invalid host %q: bad character %q", h, r)
			}
		}
	}
	return nil
}

// hopByHop headers are always removed by the proxy, so rules naming them
// would silently do nothing.
var hopByHop = []string{
	"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
	"proxy-connection", "te", "trailer", "transfer-encoding", "upgrade",
}

func validateHeaders(p *problems, path string, h Headers) {
	checkName := func(path, name string) {
		switch {
		case !isToken(name):
			p.add(path, "invalid header name %q", name)
		case slices.Contains(hopByHop, strings.ToLower(name)):
			p.add(path, "%q is a hop-by-hop header, which is always removed", name)
		}
	}
	checkSet := func(path string, set map[string]string) {
		for _, name := range slices.Sorted(maps.Keys(set)) {
			checkName(path, name)
			if strings.ContainsAny(set[name], "\r\n\x00") {
				p.add(path+"."+name, "value must not contain CR, LF or NUL")
			}
		}
	}
	checkRemove := func(path string, names []string) {
		for i, name := range names {
			checkName(fmt.Sprintf("%s[%d]", path, i), name)
		}
	}
	checkSet(path+".request_set", h.RequestSet)
	checkRemove(path+".request_remove", h.RequestRemove)
	checkSet(path+".response_set", h.ResponseSet)
	checkRemove(path+".response_remove", h.ResponseRemove)
	for i, name := range h.RequestRemove {
		if strings.EqualFold(name, "Host") {
			p.add(fmt.Sprintf("%s.request_remove[%d]", path, i), "Host cannot be removed; set it with request_set or preserve_host")
		}
	}
}

// isToken reports whether s is an RFC 9110 token, the syntax of a header
// name.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r >= 0x7f || !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func validateRetry(p *problems, path string, rt *Retry) {
	if rt.Attempts < 0 || rt.Attempts > 10 {
		p.add(path+".attempts", "must be between 0 and 10, got %d", rt.Attempts)
	}
	if rt.BudgetRatio <= 0 || rt.BudgetRatio > 1 {
		p.add(path+".budget_ratio", "must be in (0, 1], got %v", rt.BudgetRatio)
	}
	for i, cond := range rt.On {
		if !validRetryOn(cond) {
			p.add(fmt.Sprintf("%s.on[%d]", path, i), "unknown condition %q (want connect_error, timeout or a 5xx status)", cond)
		}
	}
}

func validRetryOn(s string) bool {
	if s == "connect_error" || s == "timeout" {
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 500 && n <= 599
}

func checkFile(p *problems, path, file string) {
	if file == "" {
		p.add(path, "is required")
		return
	}
	if _, err := os.Stat(file); err != nil {
		p.add(path, "%v", err)
	}
}

func checkPositive(p *problems, path string, d time.Duration) {
	if d <= 0 {
		p.add(path, "must be positive, got %v", d)
	}
}

func checkMin(p *problems, path string, n, minimum int) {
	if n < minimum {
		p.add(path, "must be at least %d, got %d", minimum, n)
	}
}
