// Package proxy forwards requests to backends.
package proxy

import (
	"maps"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
)

// hopHeaders only mean something on a single connection, so a proxy must
// not forward them (RFC 9110, section 7.6.1). Proxy-Authorization and
// Proxy-Authenticate are on the list because they carry credentials for this
// proxy, not for the backend.
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// removeHopByHop deletes the fixed hop-by-hop headers from h, plus every
// header named in Connection.
func removeHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for name := range strings.SplitSeq(v, ",") {
			if name = textproto.TrimString(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		delete(h, name)
	}
}

// cleanRequestHeaders removes hop-by-hop headers from a request going to a
// backend. "TE: trailers" is kept, because gRPC needs it to receive its
// status in trailers.
func cleanRequestHeaders(h http.Header) {
	trailers := wantsTrailers(h)
	removeHopByHop(h)
	if trailers {
		h["Te"] = []string{"trailers"}
	}
}

func wantsTrailers(h http.Header) bool {
	for _, v := range h["Te"] {
		for tok := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(textproto.TrimString(tok), "trailers") {
				return true
			}
		}
	}
	return false
}

// HeaderOps is one direction of a route's header rules: it removes headers,
// then sets others.
type HeaderOps struct {
	remove []string
	set    []headerValue
}

type headerValue struct {
	name, value string
}

// NewHeaderOps canonicalizes the header names once, so Apply does no extra
// work per request. Sets are applied in name order, so the result does not
// depend on map iteration.
func NewHeaderOps(set map[string]string, remove []string) HeaderOps {
	var ops HeaderOps
	for _, name := range remove {
		ops.remove = append(ops.remove, http.CanonicalHeaderKey(name))
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		ops.set = append(ops.set, headerValue{http.CanonicalHeaderKey(name), set[name]})
	}
	return ops
}

// Empty reports whether o changes nothing.
func (o HeaderOps) Empty() bool { return len(o.remove) == 0 && len(o.set) == 0 }

// Apply removes, then sets, the configured headers in h.
func (o HeaderOps) Apply(h http.Header) {
	for _, name := range o.remove {
		delete(h, name)
	}
	for _, hv := range o.set {
		// A fresh slice per request, so no two requests share one.
		h[hv.name] = []string{hv.value}
	}
}

// Get returns the value o sets for the canonical header name, if any. The
// proxy uses it for Host, which lives in Request.Host rather than the header
// map.
func (o HeaderOps) Get(name string) (string, bool) {
	for _, hv := range o.set {
		if hv.name == name {
			return hv.value, true
		}
	}
	return "", false
}
