package proxy

import (
	"net/http"
	"net/netip"
	"net/textproto"
	"strings"
)

// Forwarding writes the X-Forwarded-* headers (and optionally RFC 7239
// Forwarded) for requests going to backends, and finds the real client
// address.
//
// Incoming forwarding headers are kept only when the direct peer is a trusted
// proxy. From any other peer they are replaced. Otherwise a client could put
// any address in X-Forwarded-For and the backend would believe it.
type Forwarding struct {
	trusted []netip.Prefix
	rfc7239 bool
}

// NewForwarding trusts the peers in trusted. When rfc7239 is set, a Forwarded
// header is sent alongside the X-Forwarded-* headers.
func NewForwarding(trusted []netip.Prefix, rfc7239 bool) *Forwarding {
	return &Forwarding{trusted: trusted, rfc7239: rfc7239}
}

// Trusted reports whether a is a trusted proxy. a must be unmapped, as
// PeerAddr returns it.
func (f *Forwarding) Trusted(a netip.Addr) bool {
	for _, p := range f.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// PeerAddr returns the IP of the direct peer from Request.RemoteAddr, with
// IPv4-mapped IPv6 unmapped and any zone dropped. It returns the zero Addr if
// RemoteAddr is not an IP and port, as on a Unix socket.
func PeerAddr(remoteAddr string) netip.Addr {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap().WithZone("")
}

// Apply rewrites the forwarding headers in out, the header of the request
// going to the backend. peer is the direct peer, host is the Host the client
// asked for, and isTLS says whether the client connected over TLS.
func (f *Forwarding) Apply(out http.Header, peer netip.Addr, host string, isTLS bool) {
	trusted := peer.IsValid() && f.Trusted(peer)
	proto := "http"
	if isTLS {
		proto = "https"
	}

	switch {
	case !peer.IsValid():
		// There is no address to name, and nothing earlier can be trusted.
		delete(out, "X-Forwarded-For")
	case trusted && len(out["X-Forwarded-For"]) > 0:
		out.Set("X-Forwarded-For", strings.Join(out["X-Forwarded-For"], ", ")+", "+peer.String())
	default:
		out.Set("X-Forwarded-For", peer.String())
	}

	if !trusted || out.Get("X-Forwarded-Proto") == "" {
		out.Set("X-Forwarded-Proto", proto)
	}
	if !trusted || out.Get("X-Forwarded-Host") == "" {
		if host == "" {
			delete(out, "X-Forwarded-Host")
		} else {
			out.Set("X-Forwarded-Host", host)
		}
	}

	if !trusted {
		delete(out, "Forwarded")
	}
	if f.rfc7239 {
		elem := forwardedElement(peer, host, proto)
		if prior := out["Forwarded"]; len(prior) > 0 {
			elem = strings.Join(prior, ", ") + ", " + elem
		}
		out.Set("Forwarded", elem)
	}
}

// forwardedElement formats one RFC 7239 element, such as
// for="[2001:db8::1]";host=example.com;proto=https.
func forwardedElement(peer netip.Addr, host, proto string) string {
	var b strings.Builder
	b.WriteString("for=")
	switch {
	case !peer.IsValid():
		b.WriteString("unknown")
	case peer.Is6():
		b.WriteString(`"[` + peer.String() + `]"`)
	default:
		b.WriteString(peer.String())
	}
	if host != "" {
		b.WriteString(";host=")
		b.WriteString(quoteIfNeeded(host))
	}
	b.WriteString(";proto=")
	b.WriteString(proto)
	return b.String()
}

// quoteIfNeeded returns s as an RFC 9110 token if it is one, otherwise as a
// quoted string.
func quoteIfNeeded(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !isTokenChar(r) }) < 0 {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		if c := s[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func isTokenChar(r rune) bool {
	return r < 0x7f && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", r))
}

// ClientIP returns the address of the client behind any trusted proxies. in
// is the header as received, before Apply.
//
// For an untrusted peer that is the peer itself. For a trusted peer,
// X-Forwarded-For is walked from the right, skipping trusted proxies, and the
// first untrusted address wins; entries to its left were written by the
// client and may be forged. If every entry is trusted, the leftmost one is
// the client. The walk stops at an entry that is not an address and returns
// the last address it reached, so a malformed header can never produce an
// address the client chose.
func (f *Forwarding) ClientIP(in http.Header, peer netip.Addr) netip.Addr {
	if !peer.IsValid() || !f.Trusted(peer) {
		return peer
	}
	client := peer
	values := in["X-Forwarded-For"]
	for i := len(values) - 1; i >= 0; i-- {
		rest := values[i]
		for rest != "" {
			var entry string
			if j := strings.LastIndexByte(rest, ','); j >= 0 {
				entry, rest = rest[j+1:], rest[:j]
			} else {
				entry, rest = rest, ""
			}
			if textproto.TrimString(entry) == "" {
				continue
			}
			a, ok := parseXFFEntry(entry)
			if !ok {
				return client
			}
			client = a
			if !f.Trusted(a) {
				return a
			}
		}
	}
	return client
}

// parseXFFEntry accepts "1.2.3.4", "2001:db8::1", and the "1.2.3.4:5678",
// "[2001:db8::1]:443" and "[2001:db8::1]" forms some proxies write.
func parseXFFEntry(s string) (netip.Addr, bool) {
	s = textproto.TrimString(s)
	a, err := netip.ParseAddr(s)
	if err != nil {
		if ap, perr := netip.ParseAddrPort(s); perr == nil {
			a, err = ap.Addr(), nil
		} else if len(s) > 2 && s[0] == '[' && s[len(s)-1] == ']' {
			a, err = netip.ParseAddr(s[1 : len(s)-1])
		}
	}
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}
