package proxy

import (
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/MuditGarg007/prism/internal/balancer"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// randomAddrs returns n distinct random IPv4 addresses.
func randomAddrs(n int, r *rand.Rand) []netip.Addr {
	seen := map[netip.Addr]bool{}
	out := make([]netip.Addr, 0, n)
	for len(out) < n {
		var b [4]byte
		for j := range b {
			b[j] = byte(r.UintN(256))
		}
		if a := netip.AddrFrom4(b); !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// TestCanaryBucketSpread checks that shares come out close to the
// configured percent, and that raising it only adds clients.
func TestCanaryBucketSpread(t *testing.T) {
	addrs := randomAddrs(100000, rand.New(rand.NewPCG(1, 2)))
	in := func(pct float64) map[netip.Addr]bool {
		c := &Canary{Buckets: uint32(pct * 100)}
		r := httptest.NewRequest("GET", "/", nil)
		m := map[netip.Addr]bool{}
		for _, a := range addrs {
			if c.pick(r, a) {
				m[a] = true
			}
		}
		return m
	}
	five, ten := in(5), in(10)
	for pct, m := range map[float64]map[netip.Addr]bool{5: five, 10: ten} {
		share := 100 * float64(len(m)) / float64(len(addrs))
		if share < pct-0.5 || share > pct+0.5 {
			t.Errorf("%v%%: %.2f%% of clients went to the canary", pct, share)
		}
	}
	for a := range five {
		if !ten[a] {
			t.Fatalf("%v was in the canary at 5%% but not at 10%%", a)
		}
	}
	if m := in(0); len(m) != 0 {
		t.Errorf("0%%: %d clients went to the canary", len(m))
	}
	if m := in(100); len(m) != len(addrs) {
		t.Errorf("100%%: %d of %d clients went to the canary", len(m), len(addrs))
	}
	// A mapped address is the same client.
	a := netip.MustParseAddr("192.0.2.7")
	if canaryBucket(a) != canaryBucket(netip.AddrFrom16(a.As16())) {
		t.Error("mapped and plain IPv4 addresses hash differently")
	}
}

func TestCanaryOverride(t *testing.T) {
	c := &Canary{Buckets: 0, Header: "X-Canary", Cookie: "canary"}
	client := netip.MustParseAddr("192.0.2.1")
	tests := []struct {
		header, cookie string
		buckets        uint32
		want           bool
	}{
		{"", "", 0, false},
		{"", "", canaryBuckets, true},
		{"always", "", 0, true},
		{"ALWAYS", "", 0, true},
		{"never", "", canaryBuckets, false},
		{"", "always", 0, true},
		{"", "never", canaryBuckets, false},
		{"never", "always", 0, false}, // the header wins
		{"maybe", "always", 0, true},  // an unknown header value falls through
		{"", "maybe", canaryBuckets, true},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		if tt.header != "" {
			r.Header.Set("X-Canary", tt.header)
		}
		if tt.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "canary", Value: tt.cookie})
		}
		c.Buckets = tt.buckets
		if got := c.pick(r, client); got != tt.want {
			t.Errorf("header %q, cookie %q, buckets %d: canary = %v, want %v", tt.header, tt.cookie, tt.buckets, got, tt.want)
		}
	}
}

func named(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, name)
	}
}

func TestProxyCanary(t *testing.T) {
	canarySrv := httptest.NewServer(named("canary"))
	t.Cleanup(canarySrv.Close)
	pool, err := upstream.NewPool("v2", testUpstream(canarySrv.URL), balancer.New(config.BalancerRoundRobin), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rt := &Route{Canary: &Canary{Pool: pool, Header: "X-Canary"}}
	env := newCacheEnv(t, rt, named("primary"))

	env.check(t, env.do("GET", "/"), 200, "MISS", "primary", 1)
	env.check(t, env.do("GET", "/"), 200, "HIT", "primary", 1)
	// The canary is never served from the primary's cache, nor stored in it.
	for range 2 {
		w := env.do("GET", "/", "X-Canary", "always")
		env.check(t, w, 200, "", "canary", 1)
		if w.an.Pool != pool {
			t.Errorf("annotated pool %q, want v2", w.an.Pool.Name)
		}
	}
	env.check(t, env.do("GET", "/"), 200, "HIT", "primary", 1)

	rt.Canary.Buckets = canaryBuckets
	env.check(t, env.do("GET", "/"), 200, "", "canary", 1)
	env.check(t, env.do("GET", "/", "X-Canary", "never"), 200, "HIT", "primary", 1)
}

func BenchmarkCanaryPick(b *testing.B) {
	c := &Canary{Buckets: 500, Header: "X-Canary", Cookie: "canary"}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "session=abc; theme=dark")
	client := netip.MustParseAddr("192.0.2.1")
	b.ReportAllocs()
	for b.Loop() {
		c.pick(r, client)
	}
}
