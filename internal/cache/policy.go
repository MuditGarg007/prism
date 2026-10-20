package cache

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RequestCacheable reports whether a request may be answered from the cache
// and its response stored. Only GET and HEAD qualify. Range requests go
// straight through, since partial responses are not stored, and so do
// requests that ask for no-store. Other request directives, such as
// no-cache or max-age=0, are ignored, as nginx does, so clients cannot make
// the cache send every request to the backend.
func RequestCacheable(method string, h http.Header) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	if _, ok := h["Range"]; ok {
		return false
	}
	return !ParseCacheControl(h["Cache-Control"]).NoStore
}

// storableStatus lists the status codes that are stored. Others could be
// (RFC 9111 allows any status with explicit freshness), but these cover what
// a proxy cache is for.
func storableStatus(code int) bool {
	return code == http.StatusOK || code == http.StatusMovedPermanently || code == http.StatusNotFound
}

// Storable returns why the response to a GET request with header req, which
// has status code and header h and the directives cc, must not be stored, or
// "" if it may be.
//
// Freshness must be explicit. A response with none, which RFC 9111 would let
// a cache give a heuristic lifetime, is not stored: guessing how long a
// backend meant a response to live is how stale pages get served.
func Storable(req http.Header, code int, h http.Header, cc Directives) string {
	switch {
	case !storableStatus(code):
		return "status"
	case cc.NoStore:
		return "no-store"
	case cc.Private:
		return "private"
	case len(h["Set-Cookie"]) > 0:
		// A cookie set for one client must never be handed to another.
		return "set-cookie"
	case varyAll(h):
		return "vary"
	case req.Get("Authorization") != "" && !cc.Public && cc.SMaxAge == Absent && !cc.MustRevalidate:
		// RFC 9111 §3.5: a response to an authenticated request is for that
		// user only, unless the backend says otherwise.
		return "authorization"
	case !explicitFreshness(h, cc) && !(cc.NoCache && hasValidator(h)):
		return "no-freshness"
	}
	return ""
}

// varyAll reports whether h has "Vary: *", which says the response depends
// on something other than request headers, so no stored copy can match.
func varyAll(h http.Header) bool {
	for _, v := range h["Vary"] {
		for f := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(f) == "*" {
				return true
			}
		}
	}
	return false
}

func explicitFreshness(h http.Header, cc Directives) bool {
	_, expires := h["Expires"]
	return cc.SMaxAge != Absent || cc.MaxAge != Absent || expires
}

// hasValidator reports whether a stored response could be revalidated.
func hasValidator(h http.Header) bool {
	return h.Get("Etag") != "" || h.Get("Last-Modified") != ""
}

// Freshness says how old a stored response is and how long it stays fresh,
// following RFC 9111 §4.2.
type Freshness struct {
	Received   time.Time     // when the response arrived (response_time)
	InitialAge time.Duration // its age on arrival (corrected_initial_age)
	Lifetime   time.Duration // how long it is fresh for, counted from its Date
}

// NewFreshness works out the freshness of a response with header h and
// directives cc, whose request was sent at requested and whose response
// arrived at received.
func NewFreshness(h http.Header, cc Directives, requested, received time.Time) Freshness {
	date := responseDate(h, received)
	return Freshness{
		Received:   received,
		InitialAge: initialAge(h, date, requested, received),
		Lifetime:   lifetime(h, cc, date),
	}
}

// Age returns the response's current age (current_age).
func (f Freshness) Age(now time.Time) time.Duration {
	return f.InitialAge + max(0, now.Sub(f.Received))
}

// Fresh reports whether the response may be served without revalidation.
func (f Freshness) Fresh(now time.Time) bool {
	return f.Age(now) < f.Lifetime
}

// Staleness returns how long ago the response stopped being fresh, or a
// negative duration while it still is.
func (f Freshness) Staleness(now time.Time) time.Duration {
	return f.Age(now) - f.Lifetime
}

// AgeHeader formats an age as the Age header's whole seconds.
func AgeHeader(age time.Duration) string {
	return strconv.FormatInt(int64(min(max(age, 0), maxDelta*time.Second)/time.Second), 10)
}

// lifetime returns the freshness lifetime: s-maxage, else max-age, else
// Expires minus Date. no-cache makes it 0, so every use revalidates.
func lifetime(h http.Header, cc Directives, date time.Time) time.Duration {
	switch {
	case cc.NoCache:
		return 0
	case cc.SMaxAge != Absent:
		return cc.SMaxAge
	case cc.MaxAge != Absent:
		return cc.MaxAge
	}
	vv, ok := h["Expires"]
	if !ok {
		return 0
	}
	// RFC 9111 §5.3: an invalid Expires, such as "0", is in the past.
	var exp time.Time
	if len(vv) > 0 {
		exp, _ = http.ParseTime(vv[0])
	}
	if exp.IsZero() || !exp.After(date) {
		return 0
	}
	return min(exp.Sub(date), maxDelta*time.Second)
}

// responseDate returns the Date of a response, or received when it has
// none or an invalid one.
func responseDate(h http.Header, received time.Time) time.Time {
	if t, err := http.ParseTime(h.Get("Date")); err == nil {
		return t
	}
	return received
}

// initialAge implements corrected_initial_age from RFC 9111 §4.2.3: the
// larger of the age the clocks imply and the Age header plus the time the
// response spent in transit.
func initialAge(h http.Header, date, requested, received time.Time) time.Duration {
	apparent := max(0, received.Sub(date))
	corrected := ageValue(h) + max(0, received.Sub(requested))
	return max(apparent, corrected)
}

// ageValue returns the Age header's value, using the first member of a
// list. An invalid value is ignored, as RFC 9111 §5.1 says.
func ageValue(h http.Header) time.Duration {
	v := h.Get("Age")
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return parseDelta(strings.TrimSpace(v))
}
