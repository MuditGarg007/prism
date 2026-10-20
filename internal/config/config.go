package config

import "time"

type Config struct {
	// TrustedProxies lists the peers whose X-Forwarded-* headers are kept.
	// Headers from any other peer are replaced.
	TrustedProxies []Prefix `yaml:"trusted_proxies"`
	// ForwardedHeader also sends an RFC 7239 Forwarded header upstream.
	ForwardedHeader bool                 `yaml:"forwarded_header"`
	Listeners       []Listener           `yaml:"listeners"`
	Upstreams       map[string]*Upstream `yaml:"upstreams"`
	Routes          []Route              `yaml:"routes"`
	// ReadyPath, when set, is answered on every listener before routing:
	// 200 while serving, 503 once shutdown has begun.
	ReadyPath string   `yaml:"ready_path"`
	Shutdown  Shutdown `yaml:"shutdown"`
	// Cache sizes the response cache that every route with caching enabled
	// shares.
	Cache CacheStore `yaml:"cache"`
	// Admin, when set, serves the admin API on its own listener.
	Admin *Admin `yaml:"admin"`
}

type CacheStore struct {
	MaxSize ByteSize `yaml:"max_size"` // total bytes held, entries and keys included
}

type Admin struct {
	Listen string `yaml:"listen"`
}

type Shutdown struct {
	// DrainDelay is how long Prism keeps serving, reporting not ready, before
	// it stops accepting connections, so load balancers in front can notice.
	DrainDelay time.Duration `yaml:"drain_delay"`
	// Timeout bounds how long active requests get to finish once Prism stops
	// accepting connections.
	Timeout time.Duration `yaml:"timeout"`
}

type Listener struct {
	Addr     string           `yaml:"addr"`
	TLS      *ListenerTLS     `yaml:"tls"`
	Timeouts ListenerTimeouts `yaml:"timeouts"`
}

// ListenerTimeouts has no read or write timeout for the whole request,
// because either one would cut off long uploads and streaming responses.
type ListenerTimeouts struct {
	ReadHeader time.Duration `yaml:"read_header"`
	Idle       time.Duration `yaml:"idle"` // keep-alive wait between requests
}

type ListenerTLS struct {
	Certs []CertPair `yaml:"certs"`
}

type CertPair struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

const (
	BalancerRoundRobin = "round_robin"
	BalancerWeighted   = "weighted"
	BalancerLeastConn  = "least_conn"
	BalancerIPHash     = "ip_hash"
)

type Upstream struct {
	Balancer       string          `yaml:"balancer"`
	Backends       []Backend       `yaml:"backends"`
	Health         *HealthCheck    `yaml:"health"`
	CircuitBreaker *CircuitBreaker `yaml:"circuit_breaker"`
	Outlier        *Outlier        `yaml:"outlier"`
	Timeouts       Timeouts        `yaml:"timeouts"`
	// MaxIdleConnsPerHost caps idle keep-alive connections to each backend.
	// Go's default of 2 forces new connections under any real concurrency.
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"`
}

// A redact tag hides a field from the admin API's GET /config: "url" masks
// a URL's password, and "secret" hides the whole value.
type Backend struct {
	URL    string      `yaml:"url" redact:"url"`
	Weight int         `yaml:"weight"`
	TLS    *BackendTLS `yaml:"tls"`
}

type BackendTLS struct {
	ServerName string `yaml:"server_name"`
	CAFile     string `yaml:"ca_file"`
}

type HealthCheck struct {
	Path           string        `yaml:"path"`
	Interval       time.Duration `yaml:"interval"`
	Timeout        time.Duration `yaml:"timeout"`
	UnhealthyAfter int           `yaml:"unhealthy_after"`
	HealthyAfter   int           `yaml:"healthy_after"`
}

type CircuitBreaker struct {
	FailureRatio float64       `yaml:"failure_ratio"`
	MinRequests  int           `yaml:"min_requests"`
	OpenFor      time.Duration `yaml:"open_for"`
	// Window is the sliding window that failure_ratio and min_requests are
	// measured over.
	Window time.Duration `yaml:"window"`
	// HalfOpenProbes is how many requests may test a backend at once after
	// open_for has passed. All must succeed to close the breaker.
	HalfOpenProbes int `yaml:"half_open_probes"`
}

// Outlier ejects a backend after consecutive failures, for base_ejection
// times the number of times it has been ejected, up to max_ejection.
type Outlier struct {
	ConsecutiveFailures int           `yaml:"consecutive_failures"`
	BaseEjection        time.Duration `yaml:"base_ejection"`
	MaxEjection         time.Duration `yaml:"max_ejection"`
	// MaxEjectionPercent caps the share of a pool that may be ejected at
	// once, so a pool is never emptied by ejection alone.
	MaxEjectionPercent int `yaml:"max_ejection_percent"`
}

type Timeouts struct {
	Connect        time.Duration `yaml:"connect"`
	ResponseHeader time.Duration `yaml:"response_header"`
	IdleConn       time.Duration `yaml:"idle_conn"` // how long an idle backend connection is kept
}

type Route struct {
	Match          Match      `yaml:"match"`
	Upstream       string     `yaml:"upstream"`
	StripPrefix    bool       `yaml:"strip_prefix"`
	PreserveHost   bool       `yaml:"preserve_host"` // send the client's Host instead of the backend's
	Headers        Headers    `yaml:"headers"`
	RateLimit      *RateLimit `yaml:"rate_limit"`
	MaxBody        ByteSize   `yaml:"max_body"` // 0 means no limit
	MaxConcurrency int        `yaml:"max_concurrency"`
	// QueueSize is how many requests may wait for a slot once
	// max_concurrency is reached, each for up to QueueTimeout.
	QueueSize    int           `yaml:"queue_size"`
	QueueTimeout time.Duration `yaml:"queue_timeout"`
	Retry        *Retry        `yaml:"retry"`
	Cache        *Cache        `yaml:"cache"`
}

type Match struct {
	Host       string `yaml:"host"`        // exact, "*.example.com", or empty for any host
	PathPrefix string `yaml:"path_prefix"` // defaults to "/"
}

type Headers struct {
	RequestSet     map[string]string `yaml:"request_set"`
	RequestRemove  []string          `yaml:"request_remove"`
	ResponseSet    map[string]string `yaml:"response_set"`
	ResponseRemove []string          `yaml:"response_remove"`
}

type RateLimit struct {
	PerIP Rate `yaml:"per_ip"`
	Burst int  `yaml:"burst"` // defaults to the rate's count
}

type Retry struct {
	Attempts int      `yaml:"attempts"` // retries after the first try
	On       []string `yaml:"on"`       // connect_error, timeout, or a 5xx status code
	// Retries may not exceed budget_ratio of recent requests plus
	// budget_min_per_sec, so retries cannot multiply load in an outage.
	BudgetRatio     float64 `yaml:"budget_ratio"`
	BudgetMinPerSec int     `yaml:"budget_min_per_sec"`
	Backoff         Backoff `yaml:"backoff"`
}

// Backoff waits a random time in [0, min(max, base*2^n)) before retry n.
type Backoff struct {
	Base time.Duration `yaml:"base"`
	Max  time.Duration `yaml:"max"`
}

type Cache struct {
	Enabled   bool     `yaml:"enabled"`
	MaxObject ByteSize `yaml:"max_object"` // larger responses are passed through uncached
	// LockTimeout is how long a request waits for another request's fetch
	// of the same key before it fetches for itself.
	LockTimeout time.Duration `yaml:"lock_timeout"`
	// BypassHeader names a request header that, when present, skips the
	// lookup. The response may still be stored. Empty turns bypassing off.
	BypassHeader string `yaml:"bypass_header"`
	// StaleIfError caps how long past its freshness an entry may be served
	// when the backend fails. A response's stale-if-error is also capped by
	// it, so 0 never serves stale entries.
	StaleIfError time.Duration `yaml:"stale_if_error"`
}
