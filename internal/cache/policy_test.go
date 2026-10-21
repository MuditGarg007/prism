package cache

import (
	"net/http"
	"testing"
	"time"
)

func TestRequestCacheable(t *testing.T) {
	tests := []struct {
		method string
		h      http.Header
		want   bool
	}{
		{"GET", nil, true},
		{"HEAD", nil, true},
		{"POST", nil, false},
		{"GET", http.Header{"Range": {"bytes=0-9"}}, false},
		{"GET", http.Header{"Cache-Control": {"no-store"}}, false},
		{"GET", http.Header{"Cache-Control": {"no-cache, max-age=0"}}, true}, // ignored
		{"GET", http.Header{"Authorization": {"Bearer x"}}, true},            // decided by the response
	}
	for _, tt := range tests {
		if got := RequestCacheable(tt.method, tt.h); got != tt.want {
			t.Errorf("RequestCacheable(%s, %v) = %v, want %v", tt.method, tt.h, got, tt.want)
		}
	}
}

func TestStorable(t *testing.T) {
	auth := http.Header{"Authorization": {"Basic eDp5"}}
	tests := []struct {
		name string
		req  http.Header
		code int
		h    http.Header
		want string
	}{
		{"max-age", nil, 200, http.Header{"Cache-Control": {"max-age=60"}}, ""},
		{"expires", nil, 200, http.Header{"Expires": {"Wed, 21 Oct 2026 07:28:00 GMT"}}, ""},
		{"invalid expires still explicit", nil, 200, http.Header{"Expires": {"0"}}, ""},
		{"301", nil, 301, http.Header{"Cache-Control": {"max-age=60"}}, ""},
		{"404", nil, 404, http.Header{"Cache-Control": {"max-age=60"}}, ""},
		{"206", nil, 206, http.Header{"Cache-Control": {"max-age=60"}}, "status"},
		{"500", nil, 500, http.Header{"Cache-Control": {"max-age=60"}}, "status"},
		{"no freshness", nil, 200, http.Header{"Etag": {`"a"`}}, "no-freshness"},
		{"no-cache with validator", nil, 200, http.Header{"Cache-Control": {"no-cache"}, "Etag": {`"a"`}}, ""},
		{"no-cache without validator", nil, 200, http.Header{"Cache-Control": {"no-cache"}}, "no-freshness"},
		{"no-store", nil, 200, http.Header{"Cache-Control": {"max-age=60, no-store"}}, "no-store"},
		{"private", nil, 200, http.Header{"Cache-Control": {"max-age=60, private"}}, "private"},
		{"set-cookie", nil, 200, http.Header{"Cache-Control": {"max-age=60"}, "Set-Cookie": {"a=b"}}, "set-cookie"},
		{"vary star", nil, 200, http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept, *"}}, "vary"},
		{"vary names", nil, 200, http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"}}, ""},
		{"authorization", auth, 200, http.Header{"Cache-Control": {"max-age=60"}}, "authorization"},
		{"authorization public", auth, 200, http.Header{"Cache-Control": {"public, max-age=60"}}, ""},
		{"authorization s-maxage", auth, 200, http.Header{"Cache-Control": {"s-maxage=60"}}, ""},
		{"authorization must-revalidate", auth, 200, http.Header{"Cache-Control": {"must-revalidate, max-age=60"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			if req == nil {
				req = http.Header{}
			}
			cc := ParseCacheControl(tt.h["Cache-Control"])
			if got := Storable(req.Get("Authorization") != "", tt.code, tt.h, cc); got != tt.want {
				t.Errorf("Storable = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFreshnessLifetime(t *testing.T) {
	date := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	hdr := func(kv ...string) http.Header {
		h := http.Header{"Date": {date.Format(http.TimeFormat)}}
		for i := 0; i < len(kv); i += 2 {
			h.Add(kv[i], kv[i+1])
		}
		return h
	}
	in := func(d time.Duration) string { return date.Add(d).Format(http.TimeFormat) }
	tests := []struct {
		name string
		h    http.Header
		want time.Duration
	}{
		{"s-maxage over max-age", hdr("Cache-Control", "max-age=60, s-maxage=10"), 10 * time.Second},
		{"max-age over expires", hdr("Cache-Control", "max-age=60", "Expires", in(time.Hour)), time.Minute},
		{"expires minus date", hdr("Expires", in(time.Hour)), time.Hour},
		{"expires in the past", hdr("Expires", in(-time.Hour)), 0},
		{"invalid expires", hdr("Expires", "0"), 0},
		{"no-cache", hdr("Cache-Control", "no-cache, max-age=60"), 0},
		{"none", hdr(), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFreshness(tt.h, ParseCacheControl(tt.h["Cache-Control"]), date, date)
			if f.Lifetime != tt.want {
				t.Errorf("lifetime = %v, want %v", f.Lifetime, tt.want)
			}
		})
	}
}

func TestFreshnessAge(t *testing.T) {
	sent := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	got := sent.Add(2 * time.Second) // two seconds in transit
	tests := []struct {
		name string
		date time.Time
		age  string
		want time.Duration // initial age
	}{
		// Clocks agree and no Age: only the transit time counts.
		{"transit only", sent.Add(time.Second), "", 2 * time.Second},
		// The Age header plus transit is larger than what Date implies.
		{"age header", sent.Add(time.Second), "100", 102 * time.Second},
		// The backend's clock is 10 minutes behind ours.
		{"apparent age", got.Add(-10 * time.Minute), "", 10 * time.Minute},
		// A backend clock ahead of ours gives no negative age.
		{"clock ahead", got.Add(time.Hour), "", 2 * time.Second},
		{"invalid age ignored", sent, "abc", 2 * time.Second},
		{"age list uses first", sent.Add(2 * time.Second), "5, 900", 7 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{"Date": {tt.date.Format(http.TimeFormat)}, "Cache-Control": {"max-age=60"}}
			if tt.age != "" {
				h.Set("Age", tt.age)
			}
			f := NewFreshness(h, ParseCacheControl(h["Cache-Control"]), sent, got)
			if f.InitialAge != tt.want {
				t.Errorf("initial age = %v, want %v", f.InitialAge, tt.want)
			}
			if age := f.Age(got.Add(5 * time.Second)); age != tt.want+5*time.Second {
				t.Errorf("age 5s later = %v, want %v", age, tt.want+5*time.Second)
			}
		})
	}
}

func TestFreshAndStale(t *testing.T) {
	now := time.Now()
	f := Freshness{Received: now, InitialAge: 10 * time.Second, Lifetime: time.Minute}
	if !f.Fresh(now) || f.Staleness(now) != -50*time.Second {
		t.Errorf("at arrival: fresh %v, staleness %v; want fresh, -50s", f.Fresh(now), f.Staleness(now))
	}
	later := now.Add(50 * time.Second)
	if f.Fresh(later) || f.Staleness(later) != 0 {
		t.Errorf("50s later: fresh %v, staleness %v; want stale, 0", f.Fresh(later), f.Staleness(later))
	}
	if got := AgeHeader(f.Age(later)); got != "60" {
		t.Errorf("Age header = %q, want 60", got)
	}
	if got := AgeHeader(-time.Second); got != "0" {
		t.Errorf("Age header of a negative age = %q, want 0", got)
	}
	if got := AgeHeader(1 << 62); got != "2147483648" {
		t.Errorf("Age header of a huge age = %q, want the 2^31 cap", got)
	}
}
