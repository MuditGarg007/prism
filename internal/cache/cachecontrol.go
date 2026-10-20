// Package cache holds the pieces of Prism's response cache: the
// Cache-Control parser, the RFC 9111 rules for what may be stored and for
// how long, the keys, and the store itself.
package cache

import (
	"strconv"
	"strings"
	"time"
)

// Absent is the value of a Directives duration that the header did not
// give.
const Absent time.Duration = -1

// maxDelta is the largest delta-seconds a cache has to represent. RFC 9111
// §1.2.2 says a larger value, or one that overflows, counts as 2^31 seconds.
const maxDelta = 1 << 31

// Directives is a parsed Cache-Control header. Only the directives that a
// shared cache acts on are kept.
type Directives struct {
	NoStore bool
	// NoCache means a stored response must be revalidated before each use.
	// The field-name form, no-cache="Set-Cookie", is treated the same way.
	NoCache bool
	// Private keeps a response out of shared caches. The field-name form is
	// treated as private for the whole response.
	Private bool
	Public  bool
	// MustRevalidate is must-revalidate or proxy-revalidate, which mean the
	// same to a shared cache: never serve the response stale.
	MustRevalidate bool

	// Each is Absent when not given. A value that is not a valid
	// delta-seconds counts as 0, which makes the response stale, as RFC
	// 9111 §4.2.1 encourages. When a directive repeats, the smallest value
	// wins, being the most restrictive.
	MaxAge               time.Duration
	SMaxAge              time.Duration
	StaleIfError         time.Duration
	StaleWhileRevalidate time.Duration
}

// ParseCacheControl parses every Cache-Control field line in vv as one
// comma-separated list. Unknown directives are ignored, and so is anything
// it cannot parse, up to the next comma outside a quoted string.
func ParseCacheControl(vv []string) Directives {
	d := Directives{MaxAge: Absent, SMaxAge: Absent, StaleIfError: Absent, StaleWhileRevalidate: Absent}
	for _, s := range vv {
		for s != "" {
			var name, value string
			var hasValue, valid bool
			name, value, hasValue, valid, s = nextDirective(s)
			d.apply(name, value, hasValue, valid)
		}
	}
	return d
}

func (d *Directives) apply(name, value string, hasValue, valid bool) {
	switch strings.ToLower(name) {
	case "no-store":
		d.NoStore = true
	case "no-cache":
		d.NoCache = true
	case "private":
		d.Private = true
	case "public":
		d.Public = true
	case "must-revalidate", "proxy-revalidate":
		d.MustRevalidate = true
	case "max-age":
		setDelta(&d.MaxAge, value, hasValue && valid)
	case "s-maxage":
		setDelta(&d.SMaxAge, value, hasValue && valid)
	case "stale-if-error":
		setDelta(&d.StaleIfError, value, hasValue && valid)
	case "stale-while-revalidate":
		setDelta(&d.StaleWhileRevalidate, value, hasValue && valid)
	}
}

// setDelta keeps the smaller of *dst and the delta-seconds in value, which
// counts as 0 when ok is false or it is not a number.
func setDelta(dst *time.Duration, value string, ok bool) {
	v := time.Duration(0)
	if ok {
		v = parseDelta(value)
	}
	if *dst == Absent || v < *dst {
		*dst = v
	}
}

// parseDelta parses delta-seconds (1*DIGIT), capping it at 2^31 seconds. It
// returns 0 for anything else.
func parseDelta(s string) time.Duration {
	if s == "" {
		return 0
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		if n < maxDelta {
			n = n*10 + int64(c-'0')
		}
	}
	return time.Duration(min(n, maxDelta)) * time.Second
}

// nextDirective reads one directive from the start of s and returns it with
// the rest of s after its comma. valid is false when the directive had
// trailing garbage or an unterminated quoted string. name is empty when s
// started with something that is not a token.
func nextDirective(s string) (name, value string, hasValue, valid bool, rest string) {
	i := skipOWS(s, 0)
	start := i
	for i < len(s) && isTokenChar(s[i]) {
		i++
	}
	name = s[start:i]
	valid = name != ""
	i = skipOWS(s, i)
	if i < len(s) && s[i] == '=' {
		hasValue = true
		i = skipOWS(s, i+1)
		if i < len(s) && s[i] == '"' {
			var ok bool
			value, i, ok = quoted(s, i)
			valid = valid && ok
		} else {
			start := i
			for i < len(s) && isTokenChar(s[i]) {
				i++
			}
			value = s[start:i]
		}
		i = skipOWS(s, i)
	}
	if i < len(s) && s[i] != ',' {
		valid = false
	}
	return name, value, hasValue, valid, skipToComma(s, i)
}

// quoted reads the quoted-string starting at s[i], which is '"', and returns
// its unescaped value and the index just past it. ok is false when the
// string is not terminated.
func quoted(s string, i int) (value string, end int, ok bool) {
	var b strings.Builder
	for i++; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), i + 1, true
		case '\\':
			if i+1 < len(s) {
				i++
				c = s[i]
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), i, false
}

// skipToComma returns what follows the first comma at or after s[i] that is
// outside a quoted string, or "" when there is none.
func skipToComma(s string, i int) string {
	for inQuote := false; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '\\':
			if inQuote {
				i++
			}
		case ',':
			if !inQuote {
				return s[i+1:]
			}
		}
	}
	return ""
}

func skipOWS(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// isTokenChar reports whether c may appear in an RFC 9110 token.
func isTokenChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c < 0x7f && strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// String formats d as a Cache-Control value that parses back to d.
func (d Directives) String() string {
	var parts []string
	for _, f := range []struct {
		set  bool
		name string
	}{
		{d.NoStore, "no-store"},
		{d.NoCache, "no-cache"},
		{d.Private, "private"},
		{d.Public, "public"},
		{d.MustRevalidate, "must-revalidate"},
	} {
		if f.set {
			parts = append(parts, f.name)
		}
	}
	for _, f := range []struct {
		v    time.Duration
		name string
	}{
		{d.MaxAge, "max-age"},
		{d.SMaxAge, "s-maxage"},
		{d.StaleIfError, "stale-if-error"},
		{d.StaleWhileRevalidate, "stale-while-revalidate"},
	} {
		if f.v != Absent {
			parts = append(parts, f.name+"="+strconv.FormatInt(int64(f.v/time.Second), 10))
		}
	}
	return strings.Join(parts, ", ")
}
