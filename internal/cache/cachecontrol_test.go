package cache

import (
	"testing"
	"time"
)

func TestParseCacheControl(t *testing.T) {
	base := Directives{MaxAge: Absent, SMaxAge: Absent, StaleIfError: Absent, StaleWhileRevalidate: Absent}
	with := func(f func(*Directives)) Directives {
		d := base
		f(&d)
		return d
	}
	tests := []struct {
		name string
		in   []string
		want Directives
	}{
		{"empty", nil, base},
		{"max-age", []string{"max-age=60"}, with(func(d *Directives) { d.MaxAge = 60 * time.Second })},
		{"case and spaces", []string{"  Public ,\tMAX-AGE = 60 "}, with(func(d *Directives) { d.Public = true; d.MaxAge = 60 * time.Second })},
		{"quoted value", []string{`max-age="60"`}, with(func(d *Directives) { d.MaxAge = 60 * time.Second })},
		{"lines are one list", []string{"public", "s-maxage=5"}, with(func(d *Directives) { d.Public = true; d.SMaxAge = 5 * time.Second })},
		{"duplicate keeps smaller", []string{"max-age=60, max-age=10", "max-age=30"}, with(func(d *Directives) { d.MaxAge = 10 * time.Second })},
		{"invalid value is stale", []string{"max-age=abc"}, with(func(d *Directives) { d.MaxAge = 0 })},
		{"missing value is stale", []string{"max-age"}, with(func(d *Directives) { d.MaxAge = 0 })},
		{"negative is stale", []string{"max-age=-5"}, with(func(d *Directives) { d.MaxAge = 0 })},
		{"trailing garbage is stale", []string{"max-age=6 0, public"}, with(func(d *Directives) { d.MaxAge = 0; d.Public = true })},
		{"overflow caps", []string{"max-age=99999999999999999999999"}, with(func(d *Directives) { d.MaxAge = maxDelta * time.Second })},
		{"comma inside quotes", []string{`private="Set-Cookie, X-A", max-age=5`}, with(func(d *Directives) { d.Private = true; d.MaxAge = 5 * time.Second })},
		{"escaped quote", []string{`no-cache="a\", max-age=1", public`}, with(func(d *Directives) { d.NoCache = true; d.Public = true })},
		{"unterminated quote", []string{`private="abc, max-age=5`}, with(func(d *Directives) { d.Private = true })},
		{"garbage directive skipped", []string{"@@@, no-store"}, with(func(d *Directives) { d.NoStore = true })},
		{"empty members", []string{",,, ,no-cache,,"}, with(func(d *Directives) { d.NoCache = true })},
		{"proxy-revalidate", []string{"proxy-revalidate"}, with(func(d *Directives) { d.MustRevalidate = true })},
		{"stale extensions", []string{"stale-if-error=300, stale-while-revalidate=30"}, with(func(d *Directives) {
			d.StaleIfError = 300 * time.Second
			d.StaleWhileRevalidate = 30 * time.Second
		})},
		{"unknown ignored", []string{"immutable, community=\"UCI\", max-age=1"}, with(func(d *Directives) { d.MaxAge = time.Second })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseCacheControl(tt.in); got != tt.want {
				t.Errorf("ParseCacheControl(%q)\n got %+v\nwant %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDirectivesString(t *testing.T) {
	d := ParseCacheControl([]string{"public, no-cache, s-maxage=10, stale-if-error=0"})
	if got, want := d.String(), "no-cache, public, s-maxage=10, stale-if-error=0"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// FuzzCacheControl checks that parsing never panics, gives durations in
// range, and that formatting what it parsed reads back the same.
func FuzzCacheControl(f *testing.F) {
	for _, s := range []string{
		"max-age=60", `private="a, b", s-maxage="5"`, `no-cache="\"`, "public,,max-age=1 2",
		"stale-if-error=99999999999999999999", "MUST-REVALIDATE", `"`, "=,=", "max-age=\t7\t",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d := ParseCacheControl([]string{s})
		for _, v := range []time.Duration{d.MaxAge, d.SMaxAge, d.StaleIfError, d.StaleWhileRevalidate} {
			if v != Absent && (v < 0 || v > maxDelta*time.Second || v%time.Second != 0) {
				t.Fatalf("%q: duration %v out of range", s, v)
			}
		}
		if again := ParseCacheControl([]string{d.String()}); again != d {
			t.Fatalf("%q parsed to %+v, but its String %q parsed to %+v", s, d, d.String(), again)
		}
	})
}
