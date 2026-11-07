package security

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var client = netip.MustParseAddr("192.0.2.7")

func hashOf(t testing.TB, pass string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func request(hdr map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestAuthenticatorCheck(t *testing.T) {
	users := map[string]string{"alice": hashOf(t, "s3cret"), "bob": hashOf(t, "hunter2")}
	keys := map[[sha256.Size]byte]string{sha256.Sum256([]byte("key-one")): "ci"}
	tests := []struct {
		name      string
		cfg       AuthConfig
		hdr       map[string]string
		want      string // the identity, or "" for a refusal
		challenge string
	}{
		{"basic ok", AuthConfig{Realm: "api", Users: users}, map[string]string{"Authorization": basic("alice", "s3cret")}, "alice", `Basic realm="api", charset="UTF-8"`},
		{"basic wrong password", AuthConfig{Realm: "api", Users: users}, map[string]string{"Authorization": basic("alice", "hunter2")}, "", ""},
		{"basic unknown user", AuthConfig{Realm: "api", Users: users}, map[string]string{"Authorization": basic("eve", "s3cret")}, "", ""},
		{"basic missing", AuthConfig{Realm: "api", Users: users}, nil, "", ""},
		{"basic garbage", AuthConfig{Realm: "api", Users: users}, map[string]string{"Authorization": "Basic !!!"}, "", ""},
		{"key ok", AuthConfig{KeyHeader: "x-api-key", Keys: keys}, map[string]string{"X-Api-Key": "key-one"}, "ci", ""},
		{"key wrong", AuthConfig{KeyHeader: "X-API-Key", Keys: keys}, map[string]string{"X-Api-Key": "key-two"}, "", ""},
		{"key missing", AuthConfig{KeyHeader: "X-API-Key", Keys: keys}, nil, "", ""},
		{"bearer ok", AuthConfig{Realm: "api", KeyHeader: "Authorization", Keys: keys}, map[string]string{"Authorization": "bearer key-one"}, "ci", `Bearer realm="api"`},
		{"bearer needs scheme", AuthConfig{KeyHeader: "Authorization", Keys: keys}, map[string]string{"Authorization": "key-one"}, "", ""},
		{"either: key", AuthConfig{Users: users, KeyHeader: "X-API-Key", Keys: keys}, map[string]string{"X-Api-Key": "key-one"}, "ci", ""},
		{"either: basic", AuthConfig{Users: users, KeyHeader: "X-API-Key", Keys: keys}, map[string]string{"Authorization": basic("bob", "hunter2")}, "bob", ""},
		{"either: wrong key wins over good password", AuthConfig{Users: users, KeyHeader: "X-API-Key", Keys: keys},
			map[string]string{"X-Api-Key": "nope", "Authorization": basic("bob", "hunter2")}, "", ""},
		{"basic and bearer share Authorization", AuthConfig{Users: users, KeyHeader: "Authorization", Keys: keys}, map[string]string{"Authorization": basic("bob", "hunter2")}, "bob", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAuthenticator(tt.cfg)
			got, err := a.Check(request(tt.hdr), client)
			if got != tt.want {
				t.Errorf("Check = %q, %v; want %q", got, err, tt.want)
			}
			if tt.want == "" && !errors.Is(err, ErrUnauthorized) {
				t.Errorf("err = %v, want ErrUnauthorized", err)
			}
			if tt.challenge != "" && a.Challenge() != tt.challenge {
				t.Errorf("Challenge = %q, want %q", a.Challenge(), tt.challenge)
			}
		})
	}
	if c := NewAuthenticator(AuthConfig{KeyHeader: "X-API-Key", Keys: keys}).Challenge(); c != "" {
		t.Errorf("challenge for a custom key header = %q, want none", c)
	}
}

func TestAuthenticatorTwoKeyValues(t *testing.T) {
	keys := map[[sha256.Size]byte]string{sha256.Sum256([]byte("key-one")): "ci"}
	a := NewAuthenticator(AuthConfig{KeyHeader: "X-API-Key", Keys: keys})
	r := request(nil)
	r.Header["X-Api-Key"] = []string{"key-one", "key-one"}
	if _, err := a.Check(r, client); err == nil {
		t.Error("two key values were accepted")
	}
}

// TestAuthenticatorRemembers checks the cache of verified passwords: a hit
// skips bcrypt, an entry expires, and one checked against an old hash is not
// trusted after the hash changes.
func TestAuthenticatorRemembers(t *testing.T) {
	now := time.Unix(1000, 0)
	a := NewAuthenticator(AuthConfig{Users: map[string]string{"alice": hashOf(t, "s3cret")}})
	a.now = func() time.Time { return now }
	r := request(map[string]string{"Authorization": basic("alice", "s3cret")})
	if _, err := a.Check(r, client); err != nil {
		t.Fatal(err)
	}
	if len(a.verified) != 1 {
		t.Fatalf("verified = %d entries, want 1", len(a.verified))
	}
	// A hit needs no bcrypt slot: with every slot taken it still passes.
	for range cap(a.cpu) {
		a.cpu <- struct{}{}
	}
	if _, err := a.Check(r, client); err != nil {
		t.Errorf("remembered password refused: %v", err)
	}
	// A wrong password for a remembered user is not a hit, and waits.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	wrong := request(map[string]string{"Authorization": basic("alice", "nope")}).WithContext(ctx)
	if _, err := a.Check(wrong, client); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wrong password with no free slot: err = %v, want the context's", err)
	}
	for range cap(a.cpu) {
		<-a.cpu
	}

	now = now.Add(verifiedTTL)
	if a.remembered(sha256.Sum256([]byte("alice\x00s3cret")), a.cfg.Users["alice"]) {
		t.Error("entry still remembered after its TTL")
	}
	a.remember(sha256.Sum256([]byte("alice\x00s3cret")), "old-hash")
	if a.remembered(sha256.Sum256([]byte("alice\x00s3cret")), a.cfg.Users["alice"]) {
		t.Error("entry checked against another hash was trusted")
	}
}

func TestAuthenticatorFailureLimit(t *testing.T) {
	keys := map[[sha256.Size]byte]string{sha256.Sum256([]byte("key-one")): "ci"}
	a := NewAuthenticator(AuthConfig{KeyHeader: "X-API-Key", Keys: keys, FailureCount: 3, FailurePer: time.Minute})
	good := request(map[string]string{"X-Api-Key": "key-one"})
	wrong := request(map[string]string{"X-Api-Key": "key-two"})
	none := request(nil)

	// Missing credentials cost nothing.
	for range 10 {
		if _, err := a.Check(none, client); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("no credentials: err = %v, want ErrUnauthorized", err)
		}
	}
	for i := range 3 {
		if _, err := a.Check(wrong, client); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("wrong key %d: err = %v, want ErrUnauthorized", i, err)
		}
	}
	// Over the limit, even the right key is refused, without being looked at.
	var le *LimitedError
	if _, err := a.Check(good, client); !errors.As(err, &le) || le.RetryAfter <= 0 || le.RetryAfter > 20*time.Second {
		t.Fatalf("good key after 3 failures: err = %v, want a LimitedError with a wait of up to 20s", err)
	}
	if name, err := a.Check(good, netip.MustParseAddr("192.0.2.8")); name != "ci" || err != nil {
		t.Errorf("another client = %q, %v; want ci", name, err)
	}
}

func TestFailureKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"192.0.2.7", "192.0.2.7"},
		{"::ffff:192.0.2.7", "192.0.2.7"},
		{"2001:db8:1:2:aaaa::1", "2001:db8:1:2::"},
		{"2001:db8:1:2:bbbb::9", "2001:db8:1:2::"},
		{"2001:db8:1:3::1", "2001:db8:1:3::"},
	}
	for _, tt := range tests {
		if got := failureKey(netip.MustParseAddr(tt.in)); got.String() != tt.want {
			t.Errorf("failureKey(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

// A guess that passed the first look at the limit and then waited for a
// bcrypt slot, while the client's other guesses failed, is refused once it
// gets the slot rather than checked.
func TestAuthenticatorFailureLimitAfterQueue(t *testing.T) {
	a := NewAuthenticator(AuthConfig{Users: map[string]string{"alice": hashOf(t, "s3cret")}, FailureCount: 2, FailurePer: time.Minute})
	key := failureKey(client)
	a.failures.Allow(key)
	a.failures.Allow(key)
	var le *LimitedError
	if _, err := a.checkPassword(context.Background(), "alice", "s3cret", key); !errors.As(err, &le) {
		t.Errorf("checkPassword over the limit: err = %v, want a LimitedError", err)
	}
	if len(a.verified) != 0 {
		t.Error("the password was checked and remembered")
	}
}

func TestAuthenticatorCacheCap(t *testing.T) {
	now := time.Unix(1000, 0)
	a := NewAuthenticator(AuthConfig{Users: map[string]string{}})
	a.now = func() time.Time { return now }
	for i := range verifiedCap + 10 {
		a.remember(sha256.Sum256([]byte{byte(i), byte(i >> 8)}), "h")
	}
	if n := len(a.verified); n > verifiedCap {
		t.Errorf("cache holds %d entries, cap is %d", n, verifiedCap)
	}
}

func TestAuthenticatorPrepare(t *testing.T) {
	keys := map[[sha256.Size]byte]string{sha256.Sum256([]byte("k")): "ci"}
	tests := []struct {
		name string
		cfg  AuthConfig
		want http.Header
	}{
		{"strips both", AuthConfig{Users: map[string]string{}, KeyHeader: "x-api-key", Keys: keys},
			http.Header{"X-Other": {"1"}}},
		{"strips only the key header", AuthConfig{KeyHeader: "X-API-Key", Keys: keys},
			http.Header{"Authorization": {"Bearer x"}, "X-Other": {"1"}}},
		{"forwards", AuthConfig{Users: map[string]string{}, KeyHeader: "X-API-Key", Keys: keys, Forward: true},
			http.Header{"Authorization": {"Bearer x"}, "X-Api-Key": {"k"}, "X-Other": {"1"}}},
		{"identity", AuthConfig{Users: map[string]string{}, IdentityHeader: "x-auth-user"},
			http.Header{"X-Api-Key": {"k"}, "X-Other": {"1"}, "X-Auth-User": {"alice"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{"Authorization": {"Bearer x"}, "X-Api-Key": {"k"}, "X-Other": {"1"}}
			NewAuthenticator(tt.cfg).Prepare(h, "alice")
			if len(h) != len(tt.want) {
				t.Fatalf("header = %v, want %v", h, tt.want)
			}
			for k, v := range tt.want {
				if h.Get(k) != v[0] {
					t.Errorf("%s = %q, want %q", k, h.Get(k), v[0])
				}
			}
		})
	}
	h := http.Header{"X-Auth-User": {"root", "admin"}}
	NewAuthenticator(AuthConfig{Users: map[string]string{}, IdentityHeader: "X-Auth-User"}).Prepare(h, "alice")
	if got := h["X-Auth-User"]; len(got) != 1 || got[0] != "alice" {
		t.Errorf("identity header = %q, want only alice", got)
	}
}

// BenchmarkAuthenticatorRemembered is the cost of a request whose password
// was checked recently, the common case.
func BenchmarkAuthenticatorRemembered(b *testing.B) {
	a := NewAuthenticator(AuthConfig{Users: map[string]string{"alice": hashOf(b, "s3cret")}})
	r := request(map[string]string{"Authorization": basic("alice", "s3cret")})
	if _, err := a.Check(r, client); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := a.Check(r, client); err != nil {
				b.Fatal(err)
			}
		}
	})
}
