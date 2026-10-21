// Package security decides which requests may reach a route: by client IP,
// and by credentials.
package security

import "net/netip"

// IPFilter allows or refuses clients by address. It is read-only once
// built, so any number of requests may use it at once.
//
// The lists are scanned in order, at about 4 ns a prefix. That suits the
// tens of prefixes an access list holds; thousands would need a trie.
type IPFilter struct {
	allow, deny []netip.Prefix
}

// NewIPFilter returns a filter that refuses clients in deny and, when allow
// is not empty, every client outside it.
func NewIPFilter(allow, deny []netip.Prefix) *IPFilter {
	return &IPFilter{allow: allow, deny: deny}
}

// Allowed reports whether ip may pass. An invalid ip, a client whose address
// is unknown, matches no prefix: it passes a deny list but fails an allow
// list.
func (f *IPFilter) Allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if contains(f.deny, ip) {
		return false
	}
	return len(f.allow) == 0 || contains(f.allow, ip)
}

func contains(ps []netip.Prefix, ip netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
