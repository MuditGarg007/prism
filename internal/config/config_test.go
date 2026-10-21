package config

import (
	"crypto/sha256"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const examplePath = "../../examples/prism.yaml"

// minimal is the smallest valid config. Error cases are built by editing it.
const minimal = `
listeners: [{ addr: ":8080" }]
upstreams:
  web:
    backends: [{ url: "http://127.0.0.1:9000" }]
routes:
  - { match: { path_prefix: "/" }, upstream: web }
`

func TestLoadExample(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Upstreams["api"].Balancer; got != BalancerLeastConn {
		t.Errorf("api balancer = %q, want %q", got, BalancerLeastConn)
	}
	if got := cfg.Upstreams["static"].Balancer; got != BalancerRoundRobin {
		t.Errorf("static balancer = %q, want default %q", got, BalancerRoundRobin)
	}
	r := cfg.Routes[0]
	if r.MaxBody != 10<<20 {
		t.Errorf("max_body = %d, want %d", r.MaxBody, 10<<20)
	}
	if want := (Rate{Count: 100, Per: time.Second}); r.RateLimit.PerIP != want {
		t.Errorf("per_ip = %+v, want %+v", r.RateLimit.PerIP, want)
	}
	if want := []string{"connect_error", "502", "503"}; !slices.Equal(r.Retry.On, want) {
		t.Errorf("retry.on = %q, want %q", r.Retry.On, want)
	}
	if len(cfg.TrustedProxies) == 0 {
		t.Error("trusted_proxies is empty")
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal), "")
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Upstreams["web"]
	if u.Balancer != BalancerRoundRobin {
		t.Errorf("balancer = %q, want %q", u.Balancer, BalancerRoundRobin)
	}
	if u.Backends[0].Weight != 1 {
		t.Errorf("weight = %d, want 1", u.Backends[0].Weight)
	}
	if u.Timeouts.Connect != defaultConnectTimeout {
		t.Errorf("connect timeout = %v, want %v", u.Timeouts.Connect, defaultConnectTimeout)
	}
	if u.Timeouts.IdleConn != defaultIdleConnTimeout {
		t.Errorf("idle_conn timeout = %v, want %v", u.Timeouts.IdleConn, defaultIdleConnTimeout)
	}
	if u.MaxIdleConnsPerHost != defaultMaxIdleConnsPerHost {
		t.Errorf("max_idle_conns_per_host = %d, want %d", u.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	}
	lt := cfg.Listeners[0].Timeouts
	if lt.ReadHeader != defaultReadHeaderTimeout || lt.Idle != defaultListenerIdleTimeout {
		t.Errorf("listener timeouts = %+v, want read_header %v, idle %v", lt, defaultReadHeaderTimeout, defaultListenerIdleTimeout)
	}
	if cfg.TrustedProxies != nil || cfg.ForwardedHeader || cfg.Routes[0].PreserveHost {
		t.Error("trusted_proxies, forwarded_header and preserve_host must default to off")
	}
	if want := (Shutdown{Timeout: defaultShutdownTimeout}); cfg.Shutdown != want || cfg.ReadyPath != "" {
		t.Errorf("shutdown = %+v, ready_path = %q, want %+v and no ready_path", cfg.Shutdown, cfg.ReadyPath, want)
	}
	if u.Outlier != nil || u.CircuitBreaker != nil || u.Health != nil {
		t.Error("outlier, circuit_breaker and health must be off unless configured")
	}
	if r := cfg.Routes[0]; r.QueueSize != 0 || r.QueueTimeout != 0 {
		t.Errorf("queue_size = %d, queue_timeout = %v, want both zero", r.QueueSize, r.QueueTimeout)
	}
	if cfg.Cache.MaxSize != defaultCacheMaxSize || cfg.Admin != nil || cfg.Routes[0].Cache != nil {
		t.Errorf("cache = %+v, admin = %+v, route cache = %+v, want default size, no admin and no route cache",
			cfg.Cache, cfg.Admin, cfg.Routes[0].Cache)
	}
}

func TestParseCacheDefaults(t *testing.T) {
	in := strings.Replace(minimal, "upstream: web }", "upstream: web, cache: { enabled: true } }", 1)
	cfg, err := Parse([]byte(in), "")
	if err != nil {
		t.Fatal(err)
	}
	want := Cache{Enabled: true, MaxObject: defaultMaxCacheObject, LockTimeout: defaultCacheLockTimeout}
	if got := *cfg.Routes[0].Cache; got != want {
		t.Errorf("cache = %+v, want %+v", got, want)
	}
}

func TestParseResilienceDefaults(t *testing.T) {
	in := strings.Replace(minimal, "    backends:", "    circuit_breaker: {}\n    outlier: {}\n    backends:", 1)
	in = strings.Replace(in, "upstream: web }", "upstream: web, max_concurrency: 10, queue_size: 5, retry: { attempts: 1 } }", 1)
	cfg, err := Parse([]byte(in), "")
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Upstreams["web"]
	wantCB := CircuitBreaker{
		FailureRatio:   defaultFailureRatio,
		MinRequests:    defaultMinRequests,
		OpenFor:        defaultOpenFor,
		Window:         defaultBreakerWindow,
		HalfOpenProbes: defaultHalfOpenProbes,
	}
	if *u.CircuitBreaker != wantCB {
		t.Errorf("circuit_breaker = %+v, want %+v", *u.CircuitBreaker, wantCB)
	}
	wantOutlier := Outlier{
		ConsecutiveFailures: defaultConsecutiveFailures,
		BaseEjection:        defaultBaseEjection,
		MaxEjection:         defaultMaxEjection,
		MaxEjectionPercent:  defaultMaxEjectionPercent,
	}
	if *u.Outlier != wantOutlier {
		t.Errorf("outlier = %+v, want %+v", *u.Outlier, wantOutlier)
	}
	r := cfg.Routes[0]
	if r.QueueTimeout != defaultQueueTimeout {
		t.Errorf("queue_timeout = %v, want %v", r.QueueTimeout, defaultQueueTimeout)
	}
	if r.Retry.BudgetMinPerSec != defaultBudgetMinPerSec {
		t.Errorf("budget_min_per_sec = %d, want %d", r.Retry.BudgetMinPerSec, defaultBudgetMinPerSec)
	}
	if want := (Backoff{Base: defaultBackoffBase, Max: defaultBackoffMax}); r.Retry.Backoff != want {
		t.Errorf("backoff = %+v, want %+v", r.Retry.Backoff, want)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name     string
		old, new string // edit applied to minimal
		want     string // substring of the error
	}{
		{"unknown field", `upstream: web }`, `upstream: web, bogus: 1 }`, "field bogus not found"},
		{"addr without port", `":8080"`, `"8080"`, "listeners[0].addr: invalid address"},
		{"port out of range", `":8080"`, `":99999"`, "listeners[0].addr: invalid port"},
		{"missing cert file", `{ addr: ":8080" }`, `{ addr: ":8443", tls: { certs: [{ cert: nope.pem, key: nope.key }] } }`, "no such file"},
		{"duplicate listener", `{ addr: ":8080" }`, `{ addr: ":8080" }, { addr: ":8080" }`, `listeners[1].addr: duplicate listener address ":8080"`},
		{"h2c with tls", `{ addr: ":8080" }`, `{ addr: ":8443", h2c: true, tls: { certs: [{ cert: nope.pem, key: nope.key }] } }`, "listeners[0].h2c: is for listeners without TLS"},
		{"unknown balancer", "    backends:", "    balancer: random\n    backends:", `unknown balancer "random"`},
		{"bad scheme", "http://127.0.0.1:9000", "ftp://127.0.0.1:9000", "scheme must be http or https"},
		{"backend with path", "http://127.0.0.1:9000", "http://127.0.0.1:9000/api", "must not have a path"},
		{"negative weight", `9000" }`, `9000", weight: -1 }`, "weight: must be at least 1"},
		{"tls on http backend", `9000" }`, `9000", tls: { server_name: x } }`, "set on a plain http backend"},
		{"duration without unit", "    backends:", "    timeouts: { connect: 5 }\n    backends:", "cannot unmarshal !!int"},
		{"health timeout too long", "    backends:", "    health: { path: /h, interval: 1s, timeout: 2s }\n    backends:", "must be less than interval"},
		{"unknown upstream", `upstream: web }`, `upstream: nope }`, `unknown upstream "nope"`},
		{"relative path prefix", `path_prefix: "/"`, `path_prefix: "api"`, `must start with "/"`},
		{"wildcard in middle", `match: {`, `match: { host: "api.*.com",`, `"*" is only allowed`},
		{"duplicate route", `upstream: web }`, "upstream: web }\n  - { match: { path_prefix: \"/\" }, upstream: web }", "same host and path_prefix as routes[0]"},
		{"bad size unit", `upstream: web }`, `upstream: web, max_body: 10XB }`, "unit must be B, KB, MB or GB"},
		{"bad rate unit", `upstream: web }`, `upstream: web, rate_limit: { per_ip: "100/d" } }`, "unit must be s, m or h"},
		{"bad retry condition", `upstream: web }`, `upstream: web, retry: { attempts: 1, on: [404] } }`, `retry.on[0]: unknown condition "404"`},
		{"bad trusted proxy", "listeners:", "trusted_proxies: [10.0.0.0/8, 10.0.0.300]\nlisteners:", `line 2: invalid IP or CIDR "10.0.0.300"`},
		{"trusted proxy bad bits", "listeners:", "trusted_proxies: [10.0.0.0/33]\nlisteners:", `invalid CIDR "10.0.0.0/33"`},
		{"trusted proxy not a list", "listeners:", "trusted_proxies: 10.0.0.0/8\nlisteners:", "cannot unmarshal"},
		{"negative read_header", `{ addr: ":8080" }`, `{ addr: ":8080", timeouts: { read_header: -1s } }`, "listeners[0].timeouts.read_header: must be positive"},
		{"negative idle_conn", "    backends:", "    timeouts: { idle_conn: -1s }\n    backends:", "upstreams.web.timeouts.idle_conn: must be positive"},
		{"bad header name", `upstream: web }`, `upstream: web, headers: { request_set: { "X Env": prod } } }`, `headers.request_set: invalid header name "X Env"`},
		{"header value with newline", `upstream: web }`, `upstream: web, headers: { response_set: { X-A: "a\nb" } } }`, "headers.response_set.X-A: value must not contain CR, LF or NUL"},
		{"hop-by-hop header rule", `upstream: web }`, `upstream: web, headers: { response_remove: [Connection] } }`, `response_remove[0]: "Connection" is a hop-by-hop header`},
		{"remove host", `upstream: web }`, `upstream: web, headers: { request_remove: [host] } }`, "request_remove[0]: Host cannot be removed"},
		{"relative ready path", "listeners:", "ready_path: readyz\nlisteners:", `ready_path: must be a path starting with "/"`},
		{"ready path with query", "listeners:", "ready_path: /ready?x=1\nlisteners:", `ready_path: must be a path`},
		{"negative drain delay", "listeners:", "shutdown: { drain_delay: -1s }\nlisteners:", "shutdown.drain_delay: must not be negative"},
		{"negative shutdown timeout", "listeners:", "shutdown: { timeout: -1s }\nlisteners:", "shutdown.timeout: must be positive"},
		{"breaker window too short", "    backends:", "    circuit_breaker: { window: 500ms }\n    backends:", "circuit_breaker.window: must be at least 1s"},
		{"negative half open probes", "    backends:", "    circuit_breaker: { half_open_probes: -1 }\n    backends:", "circuit_breaker.half_open_probes: must be at least 1"},
		{"max ejection below base", "    backends:", "    outlier: { base_ejection: 1m, max_ejection: 30s }\n    backends:", "outlier.max_ejection: must be at least base_ejection"},
		{"ejection percent over 100", "    backends:", "    outlier: { max_ejection_percent: 101 }\n    backends:", "outlier.max_ejection_percent: must be between 1 and 100"},
		{"negative consecutive failures", "    backends:", "    outlier: { consecutive_failures: -1 }\n    backends:", "outlier.consecutive_failures: must be at least 1"},
		{"queue without concurrency", `upstream: web }`, `upstream: web, queue_size: 10 }`, "queue_size: needs max_concurrency"},
		{"negative queue size", `upstream: web }`, `upstream: web, queue_size: -1 }`, "queue_size: must be at least 0"},
		{"queue timeout without queue", `upstream: web }`, `upstream: web, queue_timeout: 1s }`, "queue_timeout: set without queue_size"},
		{"negative queue timeout", `upstream: web }`, `upstream: web, max_concurrency: 1, queue_size: 1, queue_timeout: -1s }`, "queue_timeout: must be positive"},
		{"negative budget floor", `upstream: web }`, `upstream: web, retry: { attempts: 1, budget_min_per_sec: -1 } }`, "retry.budget_min_per_sec: must be at least 1"},
		{"backoff max below base", `upstream: web }`, `upstream: web, retry: { attempts: 1, backoff: { base: 1s, max: 10ms } } }`, "retry.backoff.max: must be at least backoff.base"},
		{"max object over store", `upstream: web }`, `upstream: web, cache: { enabled: true, max_object: 128MB } }`, "cache.max_object: must not exceed cache.max_size"},
		{"negative lock timeout", `upstream: web }`, `upstream: web, cache: { enabled: true, lock_timeout: -1s } }`, "cache.lock_timeout: must be positive"},
		{"bad bypass header", `upstream: web }`, `upstream: web, cache: { enabled: true, bypass_header: "X Bypass" } }`, `cache.bypass_header: invalid header name "X Bypass"`},
		{"hop-by-hop bypass header", `upstream: web }`, `upstream: web, cache: { enabled: true, bypass_header: TE } }`, `cache.bypass_header: "TE" is a hop-by-hop header`},
		{"negative stale if error", `upstream: web }`, `upstream: web, cache: { enabled: true, stale_if_error: -1s } }`, "cache.stale_if_error: must not be negative"},
		{"admin without port", "listeners:", "admin: { listen: localhost }\nlisteners:", "admin.listen: invalid address"},
		{"admin missing listen", "listeners:", "admin: {}\nlisteners:", "admin.listen: is required"},
		{"admin on listener address", "listeners:", "admin: { listen: \":8080\" }\nlisteners:", "admin.listen: same address as listeners[0]"},
		{"negative max idle conns", "    backends:", "    max_idle_conns_per_host: -1\n    backends:", "upstreams.web.max_idle_conns_per_host: must be at least 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Replace(minimal, tt.old, tt.new, 1)
			if in == minimal {
				t.Fatalf("edit %q not found in minimal config", tt.old)
			}
			_, err := Parse([]byte(in), "")
			if err == nil {
				t.Fatalf("got no error, want one containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error:\n%v\nwant it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(nil, ""); err == nil || !strings.Contains(err.Error(), "config is empty") {
		t.Errorf("got %v, want config is empty", err)
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte("listeners: [{ addr: bad }]\nupstreams: {}\nroutes: []\n"), "")
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("got %v, want *ValidationError", err)
	}
	if len(verr.Problems) != 3 {
		t.Errorf("got %d problems, want 3:\n%v", len(verr.Problems), err)
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in   string
		want ByteSize
		ok   bool
	}{
		{"0", 0, true},
		{"512", 512, true},
		{"1KB", 1 << 10, true},
		{"10MB", 10 << 20, true},
		{"2gb", 2 << 30, true},
		{"5 MB", 5 << 20, true},
		{"", 0, false},
		{"MB", 0, false},
		{"-1MB", 0, false},
		{"1.5MB", 0, false},
		{"1TB", 0, false},
		{"99999999999GB", 0, false},
	}
	for _, tt := range tests {
		got, err := ParseByteSize(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

func TestMarshalText(t *testing.T) {
	sizes := map[ByteSize]string{0: "0B", 1500: "1500B", 1 << 10: "1KB", 1536: "1536B", 64 << 20: "64MB", 3 << 30: "3GB", 1<<30 + 1<<20: "1025MB"}
	for b, want := range sizes {
		got, _ := b.MarshalText()
		if string(got) != want {
			t.Errorf("ByteSize(%d) = %q, want %q", b, got, want)
		}
		if back, err := ParseByteSize(string(got)); err != nil || back != b {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", got, back, err, b)
		}
	}
	for _, s := range []string{"100/s", "600/m", "1/h"} {
		r, err := ParseRate(s)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := r.MarshalText(); string(got) != s {
			t.Errorf("Rate %+v = %q, want %q", r, got, s)
		}
	}
	if got, _ := (Rate{Count: 3, Per: 2 * time.Second}).MarshalText(); string(got) != "3/2s" {
		t.Errorf("odd rate = %q, want 3/2s", got)
	}
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		in   string
		want Rate
		ok   bool
	}{
		{"100/s", Rate{100, time.Second}, true},
		{"600/m", Rate{600, time.Minute}, true},
		{"1/h", Rate{1, time.Hour}, true},
		{"", Rate{}, false},
		{"100", Rate{}, false},
		{"0/s", Rate{}, false},
		{"-1/s", Rate{}, false},
		{"x/s", Rate{}, false},
		{"10/d", Rate{}, false},
	}
	for _, tt := range tests {
		got, err := ParseRate(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseRate(%q) = %+v, %v; want %+v, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

func TestParsePrefix(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" means an error
	}{
		{"10.0.0.0/8", "10.0.0.0/8"},
		{"10.1.2.3/8", "10.0.0.0/8"},
		{"127.0.0.1", "127.0.0.1/32"},
		{" 192.168.0.0/16 ", "192.168.0.0/16"},
		{"::1", "::1/128"},
		{"2001:db8::/32", "2001:db8::/32"},
		{"::ffff:10.0.0.0/104", "10.0.0.0/8"},
		{"::ffff:10.0.0.1", "10.0.0.1/32"},
		{"", ""},
		{"10.0.0.0/33", ""},
		{"10.0.0", ""},
		{"fe80::1%eth0", ""},
		{"::ffff:0.0.0.0/64", ""},
		{"example.com", ""},
	}
	for _, tt := range tests {
		got, err := ParsePrefix(tt.in)
		if tt.want == "" {
			if err == nil {
				t.Errorf("ParsePrefix(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got.String() != tt.want {
			t.Errorf("ParsePrefix(%q) = %v, %v; want %s", tt.in, got, err, tt.want)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(minimal))
	if data, err := os.ReadFile(examplePath); err == nil {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := Parse(data, "")
		if err == nil && cfg == nil {
			t.Fatal("nil config without error")
		}
	})
}

// Test credentials: the bcrypt hash is of "s3cret" at the lowest cost, and
// the digest is of "key-one".
const (
	testHash   = "$2a$04$2Waq0k1CQGjrchXQb2AjfOg3u7OIuziqivVBEfq8R8TWIsZG3IYrC"
	testDigest = "sha256:9b346041bc9a49574eb2665b2ad2a0a3f9f9cce4e42f5d1f26deb8a256b5966a"
)

// twoUpstreams is minimal with a second upstream, for canary and shadow.
const twoUpstreams = `
listeners: [{ addr: ":8080" }]
upstreams:
  web:
    backends: [{ url: "http://127.0.0.1:9000" }]
  v2:
    backends: [{ url: "http://127.0.0.1:9001" }]
routes:
  - { match: { path_prefix: "/" }, upstream: web }
`

func TestParseSecurityAndSplits(t *testing.T) {
	in := strings.Replace(twoUpstreams, `upstream: web }`, `upstream: web,
      access: { allow: [10.0.0.0/8], deny: [10.0.0.1] },
      auth: {
        basic: { users: { alice: "`+testHash+`" } },
        api_keys: { keys: { ci: "`+testDigest+`" } },
        identity_header: X-Auth-User },
      canary: { upstream: v2, percent: 2.5, header: X-Canary, cookie: canary },
      shadow: { upstream: v2 },
      websocket: {} }`, 1)
	in = "access: { deny: [192.0.2.0/24] }\n" + in
	cfg, err := Parse([]byte(in), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Access.Deny; len(got) != 1 || got[0].String() != "192.0.2.0/24" {
		t.Errorf("access.deny = %v", got)
	}
	r := cfg.Routes[0]
	if got := r.Access.Deny[0].String(); got != "10.0.0.1/32" {
		t.Errorf("route deny = %q, want 10.0.0.1/32", got)
	}
	if r.Auth.Basic.Realm != defaultRealm || r.Auth.APIKeys.Header != defaultAPIKeyHeader {
		t.Errorf("auth defaults: realm %q, header %q", r.Auth.Basic.Realm, r.Auth.APIKeys.Header)
	}
	if r.Canary.Percent != 2.5 {
		t.Errorf("canary.percent = %v, want 2.5", r.Canary.Percent)
	}
	want := Shadow{Upstream: "v2", Percent: 100, MaxBody: defaultShadowMaxBody, MaxConcurrency: defaultShadowConcurrency, Timeout: defaultShadowTimeout}
	if *r.Shadow != want {
		t.Errorf("shadow = %+v, want %+v", *r.Shadow, want)
	}
	if r.WebSocket.IdleTimeout != defaultWebSocketIdleTimeout {
		t.Errorf("websocket.idle_timeout = %v, want %v", r.WebSocket.IdleTimeout, defaultWebSocketIdleTimeout)
	}
}

func TestParseSecurityAndSplitErrors(t *testing.T) {
	tests := []struct {
		name string
		new  string // replaces "upstream: web }" in twoUpstreams
		want string
	}{
		{"empty access", `upstream: web, access: {} }`, "routes[0].access: set allow, deny or both"},
		{"bad access prefix", `upstream: web, access: { allow: [10.0.0.256] } }`, `invalid IP or CIDR "10.0.0.256"`},
		{"empty auth", `upstream: web, auth: {} }`, "routes[0].auth: set basic, api_keys or both"},
		{"no users", `upstream: web, auth: { basic: {} } }`, "auth.basic.users: at least one user is required"},
		{"not bcrypt", `upstream: web, auth: { basic: { users: { alice: plain } } } }`, "auth.basic.users.alice: not a bcrypt hash"},
		{"colon in user", `upstream: web, auth: { basic: { users: { "a:b": "` + testHash + `" } } } }`, "user name must be non-empty, without colons"},
		{"quote in realm", `upstream: web, auth: { basic: { realm: "a\"b", users: { alice: "` + testHash + `" } } } }`, "auth.basic.realm: must not contain quotes"},
		{"no keys", `upstream: web, auth: { api_keys: {} } }`, "auth.api_keys.keys: at least one key is required"},
		{"bad digest", `upstream: web, auth: { api_keys: { keys: { ci: "sha256:abc" } } } }`, `auth.api_keys.keys.ci: must be "sha256:" followed by 64 hex digits`},
		{"plain key", `upstream: web, auth: { api_keys: { keys: { ci: key-one } } } }`, "must be \"sha256:\""},
		{"duplicate key", `upstream: web, auth: { api_keys: { keys: { a: "` + testDigest + `", b: "` + testDigest + `" } } } }`, "auth.api_keys.keys.b: same key as a"},
		{"hop-by-hop key header", `upstream: web, auth: { api_keys: { header: Connection, keys: { a: "` + testDigest + `" } } } }`, `auth.api_keys.header: "Connection" is a hop-by-hop header`},
		{"identity is credentials", `upstream: web, auth: { api_keys: { keys: { a: "` + testDigest + `" } }, identity_header: x-api-key } }`, `auth.identity_header: "x-api-key" carries credentials`},
		{"identity is authorization", `upstream: web, auth: { basic: { users: { alice: "` + testHash + `" } }, identity_header: Authorization } }`, "carries credentials"},
		{"identity is host", `upstream: web, auth: { basic: { users: { alice: "` + testHash + `" } }, identity_header: Host } }`, "auth.identity_header: cannot be Host"},
		{"canary without upstream", `upstream: web, canary: { percent: 5 } }`, "canary.upstream: is required"},
		{"canary unknown upstream", `upstream: web, canary: { upstream: v3 } }`, `canary.upstream: unknown upstream "v3"`},
		{"canary own upstream", `upstream: web, canary: { upstream: web } }`, "canary.upstream: is the route's own upstream"},
		{"canary percent over 100", `upstream: web, canary: { upstream: v2, percent: 101 } }`, "canary.percent: must be between 0 and 100"},
		{"canary percent NaN", `upstream: web, canary: { upstream: v2, percent: .nan } }`, "canary.percent: must be between 0 and 100"},
		{"canary bad header", `upstream: web, canary: { upstream: v2, header: "X C" } }`, `canary.header: invalid header name "X C"`},
		{"canary bad cookie", `upstream: web, canary: { upstream: v2, cookie: "a;b" } }`, `canary.cookie: invalid cookie name "a;b"`},
		{"shadow own upstream", `upstream: web, shadow: { upstream: web } }`, "shadow.upstream: is the route's own upstream"},
		{"shadow negative percent", `upstream: web, shadow: { upstream: v2, percent: -1 } }`, "shadow.percent: must be above 0"},
		{"shadow negative concurrency", `upstream: web, shadow: { upstream: v2, max_concurrency: -1 } }`, "shadow.max_concurrency: must be at least 1"},
		{"shadow negative timeout", `upstream: web, shadow: { upstream: v2, timeout: -1s } }`, "shadow.timeout: must be positive"},
		{"websocket negative idle", `upstream: web, websocket: { idle_timeout: -1s } }`, "routes[0].websocket.idle_timeout: must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Replace(twoUpstreams, `upstream: web }`, tt.new, 1)
			_, err := Parse([]byte(in), "")
			if err == nil {
				t.Fatalf("got no error, want one containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error:\n%v\nwant it to contain %q", err, tt.want)
			}
		})
	}
	if _, err := Parse([]byte("access: {}\n"+twoUpstreams), ""); err == nil || !strings.Contains(err.Error(), "access: set allow, deny or both") {
		t.Errorf("empty top-level access: got %v", err)
	}
}

func TestParseKeyDigest(t *testing.T) {
	d, ok := ParseKeyDigest(testDigest)
	if !ok || d != sha256.Sum256([]byte("key-one")) {
		t.Errorf("ParseKeyDigest(%q) = %x, %v", testDigest, d, ok)
	}
	for _, s := range []string{"", "sha256:", "sha1:" + testDigest[7:], testDigest + "00", testDigest[:len(testDigest)-1] + "g"} {
		if _, ok := ParseKeyDigest(s); ok {
			t.Errorf("ParseKeyDigest(%q) succeeded", s)
		}
	}
}

func TestParseListenersPortZero(t *testing.T) {
	in := strings.Replace(minimal, `[{ addr: ":8080" }]`, `[{ addr: "127.0.0.1:0" }, { addr: "127.0.0.1:0", h2c: true }]`, 1)
	cfg, err := Parse([]byte(in), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Listeners) != 2 || !cfg.Listeners[1].H2C {
		t.Errorf("listeners = %+v", cfg.Listeners)
	}
}
