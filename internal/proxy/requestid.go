package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/netip"
)

// RequestIDHeader carries a request's ID to the backend and back to the
// client. The access log and error log lines record it as request_id.
const RequestIDHeader = "X-Request-Id"

// maxRequestID bounds an incoming ID, which ends up in every log line of
// the request.
const maxRequestID = 128

// requestID returns the ID of a request with header hdr from peer. An ID
// from a trusted proxy is kept, so one ID follows the request through every
// hop, as long as it is a single well-formed value. Any other request gets a
// new ID; otherwise a client could choose the ID its requests are logged
// under, and make them look like someone else's.
func (h *Handler) requestID(hdr http.Header, peer netip.Addr) string {
	if vv := hdr[RequestIDHeader]; len(vv) == 1 && validRequestID(vv[0]) && peer.IsValid() && h.fwd.Trusted(peer) {
		return vv[0]
	}
	return newRequestID()
}

// validRequestID reports whether s is 1 to maxRequestID characters of
// [A-Za-z0-9._-], which covers UUIDs and hex IDs such as Prism's own.
func validRequestID(s string) bool {
	if s == "" || len(s) > maxRequestID {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID returns 128 random bits as 32 hex digits, like nginx's
// $request_id.
func newRequestID() string {
	var b [16]byte
	rand.Read(b[:]) // never fails
	var buf [32]byte
	return string(hex.AppendEncode(buf[:0], b[:]))
}
