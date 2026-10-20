package cache

import (
	"net/http"
	"net/textproto"
	"slices"
	"strings"
)

// PrimaryKey returns the key of the resource r asks for: method, scheme,
// host and the path and query as sent, for example
// "GET https://example.com/a%2Fb?x=1".
//
// HEAD uses GET's key, since a HEAD is answered from a stored GET. The host
// is lowercased and loses a default port, so "Example.com:80" and
// "example.com" share entries. The path stays escaped, so "/a%2Fb" and
// "/a/b" do not. The query is kept as sent; reordering it could merge URLs
// that a backend treats differently.
func PrimaryKey(r *http.Request) string {
	scheme, defPort := "http", ":80"
	if r.TLS != nil {
		scheme, defPort = "https", ":443"
	}
	host := strings.TrimSuffix(strings.ToLower(r.Host), defPort)
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	var b strings.Builder
	b.Grow(len(method) + len(scheme) + len(host) + len(r.URL.RawQuery) + 16 + len(r.URL.Path))
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	b.WriteString(r.URL.EscapedPath())
	if r.URL.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(r.URL.RawQuery)
	}
	return b.String()
}

// urlPart returns the host, path and query of a primary key, dropping the
// method and scheme.
func urlPart(primary string) string {
	if _, rest, ok := strings.Cut(primary, "://"); ok {
		return rest
	}
	return primary
}

// VaryNames returns the header names in h's Vary, canonicalized, sorted and
// without duplicates, so the same set always gives the same key. The caller
// has already refused "Vary: *".
func VaryNames(h http.Header) []string {
	var names []string
	for _, v := range h["Vary"] {
		for f := range strings.SplitSeq(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				names = append(names, textproto.CanonicalMIMEHeaderKey(f))
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// VariantKey returns the key of the variant of primary that a request with
// header req selects, given the Vary names of the stored response. Without
// names it is primary itself.
//
// Each value has the whitespace around its list members removed, so
// "gzip, br" and "gzip,br" select the same variant. A missing header and an
// empty one count as the same. NUL cannot appear in a header, so it
// separates the parts without ambiguity.
func VariantKey(primary string, names []string, req http.Header) string {
	if len(names) == 0 {
		return primary
	}
	var b strings.Builder
	b.Grow(len(primary) + 64)
	b.WriteString(primary)
	for _, name := range names {
		b.WriteByte(0)
		b.WriteString(name)
		b.WriteByte(':')
		first := true
		for _, v := range req[name] {
			for f := range strings.SplitSeq(v, ",") {
				if f = strings.TrimSpace(f); f == "" {
					continue
				}
				if !first {
					b.WriteByte(',')
				}
				b.WriteString(f)
				first = false
			}
		}
	}
	return b.String()
}
