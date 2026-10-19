package config

import "time"

type Config struct {
	Listeners []Listener           `yaml:"listeners"`
	Upstreams map[string]*Upstream `yaml:"upstreams"`
	Routes    []Route              `yaml:"routes"`
}

type Listener struct {
	Addr string       `yaml:"addr"`
	TLS  *ListenerTLS `yaml:"tls"`
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
	Timeouts       Timeouts        `yaml:"timeouts"`
}

type Backend struct {
	URL    string      `yaml:"url"`
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
}

type Timeouts struct {
	Connect        time.Duration `yaml:"connect"`
	ResponseHeader time.Duration `yaml:"response_header"`
}

type Route struct {
	Match          Match      `yaml:"match"`
	Upstream       string     `yaml:"upstream"`
	StripPrefix    bool       `yaml:"strip_prefix"`
	Headers        Headers    `yaml:"headers"`
	RateLimit      *RateLimit `yaml:"rate_limit"`
	MaxBody        ByteSize   `yaml:"max_body"` // 0 means no limit
	MaxConcurrency int        `yaml:"max_concurrency"`
	Retry          *Retry     `yaml:"retry"`
	Cache          *Cache     `yaml:"cache"`
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
	Attempts    int      `yaml:"attempts"` // retries after the first try
	On          []string `yaml:"on"`       // connect_error, timeout, or a 5xx status code
	BudgetRatio float64  `yaml:"budget_ratio"`
}

type Cache struct {
	Enabled   bool     `yaml:"enabled"`
	MaxObject ByteSize `yaml:"max_object"`
}
