package admin

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/runtime"
)

func TestConfig(t *testing.T) {
	h := New(runtime.NewCurrent(stateRuntime(t)), nil, nil, discard)
	rec := do(h, "GET", "/config", "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q, body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	body := rec.Body.String()
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("%v in %s", err, body)
	}

	// path walks cfg by object keys and array indexes.
	path := func(keys ...any) any {
		var v any = cfg
		for _, k := range keys {
			switch k := k.(type) {
			case string:
				v = v.(map[string]any)[k]
			case int:
				v = v.([]any)[k]
			}
		}
		return v
	}
	tests := []struct {
		keys []any
		want any
	}{
		{[]any{"trusted_proxies"}, []any{"10.0.0.0/8", "127.0.0.1/32"}},
		{[]any{"forwarded_header"}, false},
		{[]any{"listeners", 0, "timeouts", "read_header"}, "10s"},
		{[]any{"listeners", 0, "tls"}, nil},
		{[]any{"shutdown", "timeout"}, "15s"},
		{[]any{"shutdown", "drain_delay"}, "0s"},
		{[]any{"cache", "max_size"}, "2MB"},
		{[]any{"admin"}, nil},
		{[]any{"upstreams", "web", "balancer"}, "least_conn"},
		{[]any{"upstreams", "web", "backends", 0, "url"}, "http://user:xxxxx@127.0.0.1:1"},
		{[]any{"upstreams", "web", "backends", 1, "weight"}, 3.0},
		{[]any{"upstreams", "web", "circuit_breaker", "failure_ratio"}, 0.5},
		{[]any{"upstreams", "web", "outlier", "max_ejection"}, "5m0s"},
		{[]any{"upstreams", "web", "max_idle_conns_per_host"}, 64.0},
		{[]any{"upstreams", "static", "health", "path"}, "/healthz"},
		{[]any{"upstreams", "static", "circuit_breaker"}, nil},
		{[]any{"routes", 0, "match", "host"}, "api.example.com"},
		{[]any{"routes", 0, "rate_limit"}, map[string]any{"per_ip": "100/s", "burst": 100.0}},
		{[]any{"routes", 0, "retry", "on"}, []any{"connect_error", "503"}},
		{[]any{"routes", 0, "retry", "backoff", "base"}, "25ms"},
		{[]any{"routes", 0, "queue_timeout"}, "1s"},
		{[]any{"routes", 1, "max_body"}, "1500B"},
		{[]any{"routes", 1, "cache", "stale_if_error"}, "1m0s"},
		{[]any{"routes", 1, "cache", "max_object"}, "1MB"},
		{[]any{"routes", 2, "headers", "request_set"}, map[string]any{"X-A": "1", "X-B": "2"}},
		{[]any{"routes", 2, "headers", "request_remove"}, []any{}},
		{[]any{"routes", 2, "retry"}, nil},
		{[]any{"routes", 2, "websocket", "idle_timeout"}, "1m30s"},
		{[]any{"routes", 2, "auth", "api_keys", "header"}, "X-API-Key"},
	}
	for _, tt := range tests {
		if got := path(tt.keys...); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%v = %#v, want %#v", tt.keys, got, tt.want)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Error("the backend's password is in the body")
	}
	if strings.Contains(body, "$2a$") || strings.Contains(body, "sha256:") {
		t.Error("a password hash or key digest is in the body")
	}
	if !strings.Contains(body, `"alice"`) || !strings.Contains(body, `"ci"`) {
		t.Error("user and key names are missing from the body")
	}
	// Field order follows the struct, so the file reads top to bottom.
	if i, j := strings.Index(body, `"trusted_proxies"`), strings.Index(body, `"admin"`); i != 1 || j < i {
		t.Errorf("fields out of order: trusted_proxies at %d, admin at %d", i, j)
	}
	if i, j := strings.Index(body, `"X-A"`), strings.Index(body, `"X-B"`); i > j {
		t.Error("map keys are not sorted")
	}
}

func TestEncodeConfigRedact(t *testing.T) {
	type creds struct {
		User     string   `yaml:"user"`
		Password string   `yaml:"password" redact:"secret"`
		Keys     []string `yaml:"keys" redact:"secret"`
		Token    string   `yaml:"token" redact:"secret"`
		URL      string   `yaml:"url" redact:"url"`
		Bad      string   `yaml:"bad" redact:"url"`
		Skipped  string   `yaml:"-"`
		Default  int
		hidden   int
		Wait     time.Duration     `yaml:"wait,omitempty"`
		Users    map[string]string `yaml:"users" redact:"values"`
		NoUsers  map[string]string `yaml:"no_users" redact:"values"`
	}
	v := creds{
		User:     "alice",
		Password: "s3cret",
		Keys:     []string{"k1"},
		URL:      "https://bob:pw@example.com/x",
		Bad:      "http://[::1",
		Skipped:  "no",
		Default:  7,
		hidden:   1,
		Wait:     1500 * time.Millisecond,
		Users:    map[string]string{"bob": "$2a$hash", "alice": "$2a$hash"},
	}
	var buf bytes.Buffer
	if err := encodeConfig(&buf, reflect.ValueOf(&v)); err != nil {
		t.Fatal(err)
	}
	want := `{"user":"alice","password":"[redacted]","keys":"[redacted]","token":"",` +
		`"url":"https://bob:xxxxx@example.com/x","bad":"[redacted]","default":7,"wait":"1.5s",` +
		`"users":{"alice":"[redacted]","bob":"[redacted]"},"no_users":{}}`
	if buf.String() != want {
		t.Errorf("got  %s\nwant %s", &buf, want)
	}
}
