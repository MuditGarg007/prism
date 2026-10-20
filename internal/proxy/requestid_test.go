package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestValidRequestID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"0123456789abcdef0123456789abcdef", true},
		{"6ba7b810-9dad-11d1-80b4-00c04fd430c8", true},
		{"Req_1.2-3", true},
		{strings.Repeat("a", maxRequestID), true},
		{strings.Repeat("a", maxRequestID+1), false},
		{"", false},
		{"a b", false},
		{"a\nb", false},
		{"a\"b", false},
		{"abc=", false},
		{"é", false},
	}
	for _, tt := range tests {
		if got := validRequestID(tt.id); got != tt.want {
			t.Errorf("validRequestID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestNewRequestID(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := newRequestID()
		if len(id) != 32 || !validRequestID(id) || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("newRequestID() = %q, want 32 lowercase hex digits", id)
		}
		if seen[id] {
			t.Fatalf("newRequestID() repeated %q", id)
		}
		seen[id] = true
	}
}

// TestProxyRequestID checks which incoming IDs are kept, and that the
// backend, the client and the annotations all get the same one.
func TestProxyRequestID(t *testing.T) {
	const theirs = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	loopback := netip.MustParsePrefix("127.0.0.0/8")
	tests := []struct {
		name    string
		trusted []netip.Prefix
		ids     []string // incoming X-Request-Id values
		keep    bool
	}{
		{"none", nil, nil, false},
		{"untrusted peer", nil, []string{theirs}, false},
		{"trusted peer", []netip.Prefix{loopback}, []string{theirs}, true},
		{"trusted peer, malformed", []netip.Prefix{loopback}, []string{"a b"}, false},
		{"trusted peer, two values", []netip.Prefix{loopback}, []string{theirs, theirs}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent []string
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent = r.Header.Values(RequestIDHeader)
				// A backend that echoes some other ID does not override
				// Prism's.
				w.Header().Set(RequestIDHeader, "from-backend")
			}))
			defer backend.Close()
			h := newHandler(t, testUpstream(backend.URL), &Route{}, slog.New(slog.DiscardHandler), tt.trusted...)

			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			for _, id := range tt.ids {
				r.Header.Add(RequestIDHeader, id)
			}
			w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, r)

			id := w.an.RequestID
			if tt.keep && id != theirs {
				t.Errorf("request ID %q, want the incoming %q", id, theirs)
			}
			if !tt.keep && (len(id) != 32 || id == theirs) {
				t.Errorf("request ID %q, want a new one", id)
			}
			if got := w.Header().Values(RequestIDHeader); len(got) != 1 || got[0] != id {
				t.Errorf("client got %s %q, want [%s]", RequestIDHeader, got, id)
			}
			if len(sent) != 1 || sent[0] != id {
				t.Errorf("backend got %s %q, want [%s]", RequestIDHeader, sent, id)
			}
		})
	}
}

// TestProxyRequestIDOnRejection checks that a response Prism makes itself
// still carries the ID, and that error log lines carry it too.
func TestProxyRequestIDOnRejection(t *testing.T) {
	logs, log := newTestLogger()
	h := newHandler(t, testUpstream("http://"+closedAddr(t)), &Route{PathPrefix: "/api/"}, log)
	tests := []struct {
		path   string
		status int
	}{
		{"/other", http.StatusNotFound},
		{"/api/../x", http.StatusBadRequest},
		{"/api/x", http.StatusBadGateway},
	}
	for _, tt := range tests {
		w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, httptest.NewRequest("GET", tt.path, nil))
		id := w.Header().Get(RequestIDHeader)
		if w.Code != tt.status || id == "" || id != w.an.RequestID {
			t.Errorf("%s: %d with %s %q, annotated %q; want %d with the annotated ID",
				tt.path, w.Code, RequestIDHeader, id, w.an.RequestID, tt.status)
		}
		if w.Code == http.StatusBadGateway && !strings.Contains(logs.String(), "request_id="+id) {
			t.Errorf("%s: error log %q lacks request_id=%s", tt.path, logs.String(), id)
		}
	}
}

// TestCacheRequestID checks that a hit carries its own request's ID, not the
// one of the request that filled the entry.
func TestCacheRequestID(t *testing.T) {
	env := newCacheEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RequestIDHeader, r.Header.Get(RequestIDHeader)) // echoed
		w.Write([]byte("x"))
	})
	miss, hit := env.do("GET", "/a"), env.do("GET", "/a")
	env.check(t, hit, 200, "HIT", "x", 1)
	a, b := miss.Header().Values(RequestIDHeader), hit.Header().Values(RequestIDHeader)
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] || b[0] != hit.an.RequestID {
		t.Errorf("miss got %q, hit got %q annotated %q; want one distinct ID each", a, b, hit.an.RequestID)
	}
}

var sinkID string

func BenchmarkNewRequestID(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkID = newRequestID()
	}
}
