package proxy

import (
	"net/http"
	"net/netip"
	"net/textproto"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var testTrusted = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("fd00::/8"),
}

const (
	untrustedPeer = "203.0.113.7"
	trustedPeer   = "10.0.0.5"
)

func TestForwardingApply(t *testing.T) {
	tests := []struct {
		name    string
		rfc7239 bool
		peer    string // "" means not an IP
		host    string
		tls     bool
		in      http.Header
		want    http.Header
	}{
		{
			name: "untrusted peer: spoofed headers replaced",
			peer: untrustedPeer, host: "example.com",
			in: http.Header{
				"X-Forwarded-For":   {"1.2.3.4"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"evil.test"},
				"Forwarded":         {"for=1.2.3.4"},
			},
			want: http.Header{
				"X-Forwarded-For":   {untrustedPeer},
				"X-Forwarded-Proto": {"http"},
				"X-Forwarded-Host":  {"example.com"},
			},
		},
		{
			name: "untrusted peer without headers",
			peer: untrustedPeer, host: "example.com:8443", tls: true,
			in: http.Header{},
			want: http.Header{
				"X-Forwarded-For":   {untrustedPeer},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"example.com:8443"},
			},
		},
		{
			name: "trusted peer: chain appended, proto and host kept",
			peer: trustedPeer, host: "internal:8080",
			in: http.Header{
				"X-Forwarded-For":   {"198.51.100.1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"example.com"},
				"Forwarded":         {"for=198.51.100.1"},
			},
			want: http.Header{
				"X-Forwarded-For":   {"198.51.100.1, " + trustedPeer},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"example.com"},
				"Forwarded":         {"for=198.51.100.1"},
			},
		},
		{
			name: "trusted peer: several XFF lines joined",
			peer: trustedPeer, host: "example.com",
			in: http.Header{"X-Forwarded-For": {"198.51.100.1", "198.51.100.2"}},
			want: http.Header{
				"X-Forwarded-For":   {"198.51.100.1, 198.51.100.2, " + trustedPeer},
				"X-Forwarded-Proto": {"http"},
				"X-Forwarded-Host":  {"example.com"},
			},
		},
		{
			name:    "rfc7239 from untrusted IPv6 peer",
			rfc7239: true, peer: "2001:db8::1", host: "example.com:8443", tls: true,
			in: http.Header{"Forwarded": {"for=1.2.3.4"}},
			want: http.Header{
				"X-Forwarded-For":   {"2001:db8::1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"example.com:8443"},
				"Forwarded":         {`for="[2001:db8::1]";host="example.com:8443";proto=https`},
			},
		},
		{
			name:    "rfc7239 from trusted peer appends",
			rfc7239: true, peer: trustedPeer, host: "example.com",
			in: http.Header{"Forwarded": {"for=198.51.100.1;proto=https"}},
			want: http.Header{
				"X-Forwarded-For":   {trustedPeer},
				"X-Forwarded-Proto": {"http"},
				"X-Forwarded-Host":  {"example.com"},
				"Forwarded":         {"for=198.51.100.1;proto=https, for=10.0.0.5;host=example.com;proto=http"},
			},
		},
		{
			name:    "peer is not an IP",
			rfc7239: true, peer: "", host: "",
			in: http.Header{"X-Forwarded-For": {"1.2.3.4"}, "X-Forwarded-Host": {"evil.test"}},
			want: http.Header{
				"X-Forwarded-Proto": {"http"},
				"Forwarded":         {"for=unknown;proto=http"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var peer netip.Addr
			if tt.peer != "" {
				peer = netip.MustParseAddr(tt.peer)
			}
			NewForwarding(testTrusted, tt.rfc7239).Apply(tt.in, peer, tt.host, tt.tls)
			if !reflect.DeepEqual(tt.in, tt.want) {
				t.Errorf("got  %v\nwant %v", tt.in, tt.want)
			}
			// The values Apply sets share an array, so an append to one, as
			// Header.Add does, must not write over another.
			want := tt.in.Clone()
			for k := range tt.in {
				tt.in.Add(k, "added")
				want.Add(k, "added")
			}
			if !reflect.DeepEqual(tt.in, want) {
				t.Errorf("after Add: got  %v\nwant %v", tt.in, want)
			}
		})
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name string
		peer string
		xff  []string
		want string
	}{
		{"untrusted peer ignores XFF", untrustedPeer, []string{"1.2.3.4"}, untrustedPeer},
		{"trusted peer, no XFF", trustedPeer, nil, trustedPeer},
		{"trusted peer", trustedPeer, []string{"198.51.100.1"}, "198.51.100.1"},
		{"skips trusted hops", trustedPeer, []string{"198.51.100.1, 10.0.0.2"}, "198.51.100.1"},
		{"forged left entries ignored", trustedPeer, []string{"6.6.6.6, 198.51.100.1, 10.0.0.2"}, "198.51.100.1"},
		{"several lines", trustedPeer, []string{"6.6.6.6", "198.51.100.1"}, "198.51.100.1"},
		{"all trusted gives leftmost", trustedPeer, []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.3"},
		{"garbage stops the walk", trustedPeer, []string{"6.6.6.6, garbage, 10.0.0.2"}, "10.0.0.2"},
		{"garbage right after peer", trustedPeer, []string{"6.6.6.6, unknown"}, trustedPeer},
		{"empty entries skipped", trustedPeer, []string{"198.51.100.1, , "}, "198.51.100.1"},
		{"ipv4 with port", trustedPeer, []string{"198.51.100.1:5555"}, "198.51.100.1"},
		{"bracketed ipv6 with port", trustedPeer, []string{"[2001:db8::1]:443"}, "2001:db8::1"},
		{"bracketed ipv6", trustedPeer, []string{"[2001:db8::1]"}, "2001:db8::1"},
		{"ipv4-mapped is unmapped", trustedPeer, []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"trusted ipv6 hop", "fd00::1", []string{"198.51.100.1, fd00::2"}, "198.51.100.1"},
	}
	f := NewForwarding(testTrusted, false)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.xff != nil {
				h["X-Forwarded-For"] = tt.xff
			}
			if got := f.ClientIP(h, netip.MustParseAddr(tt.peer)); got.String() != tt.want {
				t.Errorf("ClientIP = %v, want %s", got, tt.want)
			}
		})
	}
}

func TestPeerAddr(t *testing.T) {
	tests := map[string]string{
		"192.0.2.1:1234":        "192.0.2.1",
		"[2001:db8::1]:80":      "2001:db8::1",
		"[::ffff:192.0.2.1]:80": "192.0.2.1",
		"[fe80::1%eth0]:80":     "fe80::1",
		"":                      "invalid IP",
		"@":                     "invalid IP",
		"192.0.2.1":             "invalid IP",
	}
	for in, want := range tests {
		if got := PeerAddr(in); got.String() != want {
			t.Errorf("PeerAddr(%q) = %v, want %s", in, got, want)
		}
	}
}

func TestQuoteIfNeeded(t *testing.T) {
	tests := map[string]string{
		"example.com":      "example.com",
		"example.com:8443": `"example.com:8443"`,
		`a"b\c`:            `"a\"b\\c"`,
		"":                 `""`,
	}
	for in, want := range tests {
		if got := quoteIfNeeded(in); got != want {
			t.Errorf("quoteIfNeeded(%q) = %s, want %s", in, got, want)
		}
	}
}

// refClientIP is a simpler version of ClientIP used as the fuzzing oracle.
func refClientIP(f *Forwarding, xff []string, peer netip.Addr) netip.Addr {
	if !f.Trusted(peer) {
		return peer
	}
	var entries []string
	for _, v := range xff {
		entries = append(entries, strings.Split(v, ",")...)
	}
	client := peer
	for _, e := range slices.Backward(entries) {
		if textproto.TrimString(e) == "" {
			continue
		}
		a, ok := parseXFFEntry(e)
		if !ok {
			return client
		}
		if client = a; !f.Trusted(a) {
			return a
		}
	}
	return client
}

func FuzzClientIP(f *testing.F) {
	f.Add("198.51.100.1, 10.0.0.2", "6.6.6.6", true)
	f.Add("[2001:db8::1]:443,,garbage", "", true)
	f.Add("::ffff:10.1.1.1, fd00::9", "10.0.0.1", false)
	fw := NewForwarding(testTrusted, true)
	f.Fuzz(func(t *testing.T, line1, line2 string, trustedPeerConn bool) {
		peer := netip.MustParseAddr(untrustedPeer)
		if trustedPeerConn {
			peer = netip.MustParseAddr(trustedPeer)
		}
		xff := []string{line1, line2}
		got := fw.ClientIP(http.Header{"X-Forwarded-For": xff}, peer)
		if want := refClientIP(fw, xff, peer); got != want {
			t.Fatalf("ClientIP = %v, reference says %v", got, want)
		}
		if !got.IsValid() || got.Zone() != "" || got.Is4In6() {
			t.Fatalf("ClientIP returned %v", got)
		}

		// Apply must never panic, and must not keep a forged chain from an
		// untrusted peer.
		h := http.Header{"X-Forwarded-For": xff, "Forwarded": {line1}}
		fw.Apply(h, peer, line2, false)
		if !trustedPeerConn && h.Get("X-Forwarded-For") != untrustedPeer {
			t.Fatalf("untrusted peer kept X-Forwarded-For %q", h.Get("X-Forwarded-For"))
		}
	})
}

func BenchmarkClientIP(b *testing.B) {
	f := NewForwarding(testTrusted, false)
	h := http.Header{"X-Forwarded-For": {"198.51.100.1, 10.0.0.3, 10.0.0.2"}}
	peer := netip.MustParseAddr(trustedPeer)
	b.ReportAllocs()
	for b.Loop() {
		f.ClientIP(h, peer)
	}
}
