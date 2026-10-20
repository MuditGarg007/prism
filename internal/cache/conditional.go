package cache

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

// NotModified reports whether a GET or HEAD with header req can be answered
// 304 from a stored response with header h (RFC 9110 §13.1.2, §13.1.3).
// If-None-Match is compared weakly against the ETag; If-Modified-Since
// counts only without it, and only against a valid Last-Modified.
func NotModified(req, h http.Header) bool {
	if inm, ok := req["If-None-Match"]; ok {
		return etagListMatch(inm, h.Get("Etag"))
	}
	ims, err := http.ParseTime(req.Get("If-Modified-Since"))
	if err != nil {
		return false
	}
	lm, err := http.ParseTime(h.Get("Last-Modified"))
	return err == nil && !lm.After(ims)
}

// etagListMatch reports whether an If-None-Match list matches etag by weak
// comparison. "*" matches any stored response. A list that does not parse
// matches nothing from the first bad member on, so the full response is sent.
func etagListMatch(list []string, etag string) bool {
	want, ok := opaqueTag(etag)
	if !ok {
		want = ""
	}
	for _, v := range list {
		for {
			v = strings.TrimLeft(v, " \t,")
			if v == "" {
				break
			}
			if v[0] == '*' {
				return true
			}
			tag, rest, ok := scanETag(v)
			if !ok {
				break
			}
			if want != "" && tag == want {
				return true
			}
			v = rest
		}
	}
	return false
}

// opaqueTag returns an ETag value without any W/ prefix, quotes included.
func opaqueTag(etag string) (string, bool) {
	tag, rest, ok := scanETag(strings.TrimSpace(etag))
	return tag, ok && rest == ""
}

// scanETag reads one entity-tag from the start of s and returns its opaque
// part, quotes included, and what follows it. A comma may appear inside
// the quotes, so lists cannot simply be split on commas.
func scanETag(s string) (tag, rest string, ok bool) {
	s = strings.TrimPrefix(s, "W/")
	if len(s) < 2 || s[0] != '"' {
		return "", "", false
	}
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return s[:i+1], s[i+1:], true
		case c == 0x21 || (c >= 0x23 && c <= 0x7e) || c >= 0x80:
		default:
			return "", "", false
		}
	}
	return "", "", false
}

// notModifiedHeaders are the stored header fields a 304 carries (RFC 9110
// §15.4.5). The rest describe a body the 304 does not have.
var notModifiedHeaders = []string{"Cache-Control", "Content-Location", "Date", "Etag", "Expires", "Last-Modified", "Vary"}

// NotModifiedHeader reports whether a 304 sent from a stored response
// carries the canonical header name.
func NotModifiedHeader(name string) bool {
	return slices.Contains(notModifiedHeaders, name)
}

// Revalidated returns the entry that replaces e after the backend answered
// its revalidation with a 304 whose header is h. As RFC 9111 §3.2 says, h's
// fields replace e's, except Content-Length, which belongs to the 304. Age
// is dropped unless h has one, since the old value no longer applies. The
// freshness is worked out again, and the body is shared.
func Revalidated(e *Entry, h http.Header, requested, received time.Time) *Entry {
	merged := e.Header.Clone()
	delete(merged, "Age")
	for k, vv := range h {
		if k == "Content-Length" {
			continue
		}
		// Capped, like Clone's slices, so an append by whoever serves the
		// entry cannot write into a shared array.
		merged[k] = slices.Clip(slices.Clone(vv))
	}
	cc := ParseCacheControl(merged["Cache-Control"])
	return &Entry{
		Key:       e.Key,
		Primary:   e.Primary,
		Vary:      e.Vary,
		Status:    e.Status,
		Header:    merged,
		Body:      e.Body,
		CC:        cc,
		Freshness: NewFreshness(merged, cc, requested, received),
	}
}
