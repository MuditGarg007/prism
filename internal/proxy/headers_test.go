package proxy

import (
	"net/http"
	"reflect"
	"testing"
)

func TestCleanRequestHeaders(t *testing.T) {
	tests := []struct {
		name string
		in   http.Header
		want http.Header
	}{
		{
			name: "fixed list",
			in: http.Header{
				"Connection":          {"keep-alive"},
				"Keep-Alive":          {"timeout=5"},
				"Proxy-Connection":    {"keep-alive"},
				"Proxy-Authorization": {"Basic Zm9vOmJhcg=="},
				"Transfer-Encoding":   {"chunked"},
				"Trailer":             {"X-Checksum"},
				"Upgrade":             {"websocket"},
				"Accept":              {"*/*"},
			},
			want: http.Header{"Accept": {"*/*"}},
		},
		{
			name: "headers named in Connection",
			in: http.Header{
				"Connection": {"close, x-secret ", "X-Other"},
				"X-Secret":   {"1"},
				"X-Other":    {"2"},
				"X-Kept":     {"3"},
			},
			want: http.Header{"X-Kept": {"3"}},
		},
		{
			name: "TE trailers is kept alone",
			in:   http.Header{"Te": {"deflate, Trailers"}},
			want: http.Header{"Te": {"trailers"}},
		},
		{
			name: "other TE is removed",
			in:   http.Header{"Te": {"gzip"}},
			want: http.Header{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanRequestHeaders(tt.in)
			if !reflect.DeepEqual(tt.in, tt.want) {
				t.Errorf("got %v, want %v", tt.in, tt.want)
			}
		})
	}
}

func TestHeaderOps(t *testing.T) {
	ops := NewHeaderOps(
		map[string]string{"x-env": "prod", "Host": "api.internal", "X-Multi": "one"},
		[]string{"server", "X-Powered-By"},
	)
	h := http.Header{
		"Server":       {"nginx"},
		"X-Powered-By": {"php"},
		"X-Multi":      {"a", "b"},
		"X-Kept":       {"yes"},
	}
	ops.Apply(h)
	want := http.Header{
		"X-Env":   {"prod"},
		"Host":    {"api.internal"},
		"X-Multi": {"one"},
		"X-Kept":  {"yes"},
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("got %v, want %v", h, want)
	}
	if v, ok := ops.Get("Host"); !ok || v != "api.internal" {
		t.Errorf("Get(Host) = %q, %v", v, ok)
	}
	if _, ok := ops.Get("X-None"); ok {
		t.Error("Get found a header that is not set")
	}
	if ops.Empty() || !NewHeaderOps(nil, nil).Empty() {
		t.Error("Empty is wrong")
	}
}

func TestHeaderOpsDoNotShareValues(t *testing.T) {
	ops := NewHeaderOps(map[string]string{"X-Env": "prod"}, nil)
	a, b := http.Header{}, http.Header{}
	ops.Apply(a)
	ops.Apply(b)
	a["X-Env"][0] = "changed"
	if b.Get("X-Env") != "prod" {
		t.Error("two requests share one header value slice")
	}
}
