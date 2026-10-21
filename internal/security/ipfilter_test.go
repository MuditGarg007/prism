package security

import (
	"net/netip"
	"testing"
)

func prefixes(ss ...string) []netip.Prefix {
	ps := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		ps[i] = netip.MustParsePrefix(s)
	}
	return ps
}

func TestIPFilter(t *testing.T) {
	tests := []struct {
		name        string
		allow, deny []netip.Prefix
		ip          string // "" for an unknown client
		want        bool
	}{
		{"deny only, outside", nil, prefixes("10.0.0.0/8"), "192.0.2.1", true},
		{"deny only, inside", nil, prefixes("10.0.0.0/8"), "10.1.2.3", false},
		{"allow only, inside", prefixes("10.0.0.0/8"), nil, "10.1.2.3", true},
		{"allow only, outside", prefixes("10.0.0.0/8"), nil, "192.0.2.1", false},
		{"deny wins over allow", prefixes("10.0.0.0/8"), prefixes("10.0.0.1/32"), "10.0.0.1", false},
		{"allowed next to a denied host", prefixes("10.0.0.0/8"), prefixes("10.0.0.1/32"), "10.0.0.2", true},
		{"second allow prefix", prefixes("10.0.0.0/8", "2001:db8::/32"), nil, "2001:db8::1", true},
		{"mapped address", prefixes("10.0.0.0/8"), nil, "::ffff:10.0.0.1", true},
		{"v4 prefix does not match v6", nil, prefixes("0.0.0.0/0"), "2001:db8::1", true},
		{"unknown client, deny list", nil, prefixes("10.0.0.0/8"), "", true},
		{"unknown client, allow list", prefixes("0.0.0.0/0", "::/0"), nil, "", false},
		{"empty filter", nil, nil, "192.0.2.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ip netip.Addr
			if tt.ip != "" {
				ip = netip.MustParseAddr(tt.ip)
			}
			if got := NewIPFilter(tt.allow, tt.deny).Allowed(ip); got != tt.want {
				t.Errorf("Allowed(%v) = %v, want %v", ip, got, tt.want)
			}
		})
	}
}

func BenchmarkIPFilter(b *testing.B) {
	var allow []netip.Prefix
	for i := range 32 {
		allow = append(allow, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i), 0, 0}), 16))
	}
	f := NewIPFilter(allow, prefixes("192.0.2.0/24"))
	ip := netip.MustParseAddr("10.31.4.5")
	b.ReportAllocs()
	for b.Loop() {
		if !f.Allowed(ip) {
			b.Fatal("refused")
		}
	}
}
