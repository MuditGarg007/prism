package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/MuditGarg007/prism/internal/security"
	"golang.org/x/crypto/bcrypt"
)

// sendFrom serves a GET for path through h from a trusted proxy on behalf
// of client.
func sendFrom(h http.Handler, client, path string, hdr http.Header) *annotatedRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	for k, vv := range hdr {
		req.Header[k] = vv
	}
	req.Header.Set("X-Forwarded-For", client)
	w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, req)
	return w
}

func TestProxyAccess(t *testing.T) {
	backend, hits := countingBackend(t)
	trusted := netip.MustParsePrefix("127.0.0.1/32")
	h := newHandler(t, testUpstream(backend.URL), &Route{
		PathPrefix: "/api/",
		Access:     security.NewIPFilter([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, nil),
	}, slog.New(slog.DiscardHandler), trusted)
	h.access = security.NewIPFilter(nil, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})

	tests := []struct {
		client, path string
		want         int
		route        bool // whether the route is annotated
	}{
		{"10.0.0.1", "/api/x", http.StatusOK, true},
		{"198.51.100.1", "/api/x", http.StatusForbidden, true}, // outside the route's allow list
		{"192.0.2.1", "/api/x", http.StatusForbidden, false},   // denied before routing
		{"192.0.2.1", "/nowhere", http.StatusForbidden, false}, // even where no route matches
		{"198.51.100.1", "/nowhere", http.StatusNotFound, false},
	}
	for _, tt := range tests {
		w := sendFrom(h, tt.client, tt.path, nil)
		if w.Code != tt.want {
			t.Errorf("%s %s: got %d, want %d", tt.client, tt.path, w.Code, tt.want)
		}
		if tt.want == http.StatusForbidden && w.an.Error != "forbidden" {
			t.Errorf("%s %s: error = %q, want forbidden", tt.client, tt.path, w.an.Error)
		}
		if (w.an.Route != nil) != tt.route {
			t.Errorf("%s %s: route annotated = %v, want %v", tt.client, tt.path, w.an.Route != nil, tt.route)
		}
		if w.Header().Get(RequestIDHeader) == "" {
			t.Errorf("%s %s: no request ID", tt.client, tt.path)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("backend got %d requests, want 1", n)
	}
}

func testAuthenticator(t testing.TB, forward bool) *security.Authenticator {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return security.NewAuthenticator(security.AuthConfig{
		Realm:          "api",
		Users:          map[string]string{"alice": string(h)},
		KeyHeader:      "X-API-Key",
		Keys:           map[[sha256.Size]byte]string{sha256.Sum256([]byte("key-one")): "ci"},
		Forward:        forward,
		IdentityHeader: "X-Auth-User",
	})
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestProxyAuth(t *testing.T) {
	var last atomic.Pointer[http.Header]
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		last.Store(&h)
	}))
	t.Cleanup(backend.Close)

	for _, forward := range []bool{false, true} {
		last.Store(nil)
		h := newHandler(t, testUpstream(backend.URL), &Route{Auth: testAuthenticator(t, forward)}, slog.New(slog.DiscardHandler))

		w := sendFrom(h, "198.51.100.1", "/", http.Header{"X-Auth-User": {"root"}})
		if w.Code != http.StatusUnauthorized || w.an.Error != "unauthorized" {
			t.Errorf("no credentials: got %d, error %q; want 401 unauthorized", w.Code, w.an.Error)
		}
		if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="api", charset="UTF-8"` {
			t.Errorf("WWW-Authenticate = %q", got)
		}
		w = sendFrom(h, "198.51.100.1", "/", http.Header{"Authorization": {basicAuth("alice", "nope")}})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("wrong password: got %d, want 401", w.Code)
		}
		if last.Load() != nil {
			t.Fatal("a refused request reached the backend")
		}

		for _, c := range []struct {
			hdr  http.Header
			user string
		}{
			{http.Header{"Authorization": {basicAuth("alice", "s3cret")}, "X-Auth-User": {"root"}}, "alice"},
			{http.Header{"X-Api-Key": {"key-one"}}, "ci"},
		} {
			w := sendFrom(h, "198.51.100.1", "/", c.hdr)
			if w.Code != http.StatusOK || w.an.User != c.user {
				t.Errorf("forward=%v %v: got %d, user %q; want 200, %q", forward, c.hdr, w.Code, w.an.User, c.user)
				continue
			}
			got := *last.Load()
			if v := got["X-Auth-User"]; len(v) != 1 || v[0] != c.user {
				t.Errorf("forward=%v: backend got X-Auth-User %q, want only %q", forward, v, c.user)
			}
			for k := range c.hdr {
				if k == "X-Auth-User" {
					continue
				}
				if _, sent := got[k]; sent != forward {
					t.Errorf("forward=%v: backend got %s = %v", forward, k, sent)
				}
			}
		}
	}
}

// TestProxyAuthCache checks that a response to a request let in by the
// route's auth is cached only when the backend marks it shareable, as with
// an Authorization header.
func TestProxyAuthCache(t *testing.T) {
	for _, tt := range []struct {
		cc   string
		want string // X-Cache of the second request
		hits int64
	}{
		{"max-age=60", "MISS", 2},
		{"public, max-age=60", "HIT", 1},
		{"s-maxage=60", "HIT", 1},
	} {
		env := newCacheEnv(t, &Route{Auth: testAuthenticator(t, false)}, fixed(tt.cc, "x"))
		env.do("GET", "/", "X-API-Key", "key-one")
		env.check(t, env.do("GET", "/", "X-API-Key", "key-one"), 200, tt.want, "x", tt.hits)
		// A hit still needs credentials: auth comes before the cache.
		if w := env.do("GET", "/"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: unauthenticated request got %d, want 401", tt.cc, w.Code)
		}
	}
}
