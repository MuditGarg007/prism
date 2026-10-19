package config

import (
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
