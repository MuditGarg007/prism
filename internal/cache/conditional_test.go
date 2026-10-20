package cache

import (
	"net/http"
	"testing"
	"time"
)

func TestNotModified(t *testing.T) {
	stored := http.Header{
		"Etag":          {`W/"v1"`},
		"Last-Modified": {"Tue, 20 Oct 2026 12:00:00 GMT"},
	}
	tests := []struct {
		name string
		req  http.Header
		want bool
	}{
		{"no conditionals", http.Header{}, false},
		{"etag matches weakly", http.Header{"If-None-Match": {`"v1"`}}, true},
		{"weak against weak", http.Header{"If-None-Match": {`W/"v1"`}}, true},
		{"etag differs", http.Header{"If-None-Match": {`"v2"`}}, false},
		{"etag in a list", http.Header{"If-None-Match": {`"a", W/"v1"`}}, true},
		{"etag in a second line", http.Header{"If-None-Match": {`"a"`, `"v1"`}}, true},
		{"star", http.Header{"If-None-Match": {"*"}}, true},
		{"unquoted is invalid", http.Header{"If-None-Match": {"v1"}}, false},
		{"bad member stops the list", http.Header{"If-None-Match": {`bad, "v1"`}}, false},
		{"case matters", http.Header{"If-None-Match": {`"V1"`}}, false},
		// If-None-Match wins, even when If-Modified-Since would match.
		{"etag differs, date matches", http.Header{
			"If-None-Match":     {`"v2"`},
			"If-Modified-Since": {"Tue, 20 Oct 2026 12:00:00 GMT"},
		}, false},
		{"same date", http.Header{"If-Modified-Since": {"Tue, 20 Oct 2026 12:00:00 GMT"}}, true},
		{"later date", http.Header{"If-Modified-Since": {"Tue, 20 Oct 2026 13:00:00 GMT"}}, true},
		{"earlier date", http.Header{"If-Modified-Since": {"Tue, 20 Oct 2026 11:59:59 GMT"}}, false},
		{"invalid date", http.Header{"If-Modified-Since": {"yesterday"}}, false},
	}
	for _, tt := range tests {
		if got := NotModified(tt.req, stored); got != tt.want {
			t.Errorf("%s: NotModified = %v, want %v", tt.name, got, tt.want)
		}
	}

	// Without validators, nothing matches except "*".
	if NotModified(http.Header{"If-None-Match": {`""`}}, http.Header{}) {
		t.Error(`"" matched a response without an ETag`)
	}
	if NotModified(http.Header{"If-Modified-Since": {"Tue, 20 Oct 2026 12:00:00 GMT"}}, http.Header{}) {
		t.Error("If-Modified-Since matched a response without Last-Modified")
	}
	if !NotModified(http.Header{"If-None-Match": {"*"}}, http.Header{}) {
		t.Error("* did not match a stored response")
	}
}

func TestScanETag(t *testing.T) {
	tests := []struct {
		in, tag, rest string
		ok            bool
	}{
		{`"abc"`, `"abc"`, "", true},
		{`W/"abc", "d"`, `"abc"`, `, "d"`, true},
		{`"a,b"`, `"a,b"`, "", true}, // a comma inside the quotes
		{`""`, `""`, "", true},
		{`"abc`, "", "", false},
		{`abc`, "", "", false},
		{"\"a b\"", "", "", false}, // a space is not an etagc
		{`"a"b"`, `"a"`, `b"`, true},
	}
	for _, tt := range tests {
		tag, rest, ok := scanETag(tt.in)
		if tag != tt.tag || rest != tt.rest || ok != tt.ok {
			t.Errorf("scanETag(%q) = %q, %q, %v; want %q, %q, %v", tt.in, tag, rest, ok, tt.tag, tt.rest, tt.ok)
		}
	}
}

func TestRevalidated(t *testing.T) {
	sent := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	h := http.Header{
		"Cache-Control":  {"max-age=60"},
		"Content-Length": {"5"},
		"Content-Type":   {"text/plain"},
		"Date":           {sent.Format(http.TimeFormat)},
		"Etag":           {`"v1"`},
		"Age":            {"30"},
		"Vary":           {"Accept"},
	}
	req := http.Header{"Accept": {"text/plain"}}
	cc := ParseCacheControl(h["Cache-Control"])
	old := NewEntry("GET http://a/x", req, 200, h, []byte("hello"), cc, NewFreshness(h, cc, sent, sent))

	later := sent.Add(2 * time.Minute)
	notMod := http.Header{
		"Cache-Control":  {"max-age=300"},
		"Content-Length": {"0"},
		"Date":           {later.Format(http.TimeFormat)},
		"Etag":           {`"v1"`},
	}
	e := Revalidated(old, notMod, later, later)

	if e.Key != old.Key || e.Primary != old.Primary || &e.Body[0] != &old.Body[0] {
		t.Error("the merged entry has a new key or a copied body")
	}
	if got := e.Header.Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q, want the stored 5", got)
	}
	if got := e.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want the stored value", got)
	}
	if _, ok := e.Header["Age"]; ok {
		t.Error("the stored Age survived a 304 without one")
	}
	if e.CC.MaxAge != 300*time.Second || e.Freshness.Lifetime != 300*time.Second {
		t.Errorf("max-age %v, lifetime %v; want the 304's 5m", e.CC.MaxAge, e.Freshness.Lifetime)
	}
	if !e.Freshness.Fresh(later) || e.Freshness.Age(later) != 0 {
		t.Errorf("merged entry: fresh %v, age %v; want fresh, 0", e.Freshness.Fresh(later), e.Freshness.Age(later))
	}
	if old.Header.Get("Cache-Control") != "max-age=60" || old.Header.Get("Age") != "30" {
		t.Error("Revalidated changed the old entry")
	}
	// An append to a merged value must not write into the 304's array.
	vv := e.Header["Etag"]
	if cap(vv) != len(vv) {
		t.Errorf("merged value has spare capacity %d", cap(vv)-len(vv))
	}
}

func TestStoreLookup(t *testing.T) {
	s := NewStore(1 << 20)
	req := http.Header{"Accept": {"a"}}
	if key, _, ok := s.Lookup("GET http://a/", req); ok || key != "GET http://a/" {
		t.Errorf("empty store: key %q, found %v; want the primary key, false", key, ok)
	}
	h := http.Header{"Vary": {"Accept"}, "Cache-Control": {"max-age=60"}}
	e := NewEntry("GET http://a/", req, 200, h, nil, Directives{}, Freshness{})
	s.Put(e)
	if key, got, ok := s.Lookup("GET http://a/", req); !ok || got != e || key != e.Key {
		t.Errorf("stored variant: key %q, found %v", key, ok)
	}
	other := http.Header{"Accept": {"b"}}
	if key, _, ok := s.Lookup("GET http://a/", other); ok || key != VariantKey("GET http://a/", e.Vary, other) {
		t.Errorf("other variant: key %q, found %v; want its own variant key, false", key, ok)
	}
}

func BenchmarkNotModified(b *testing.B) {
	stored := http.Header{"Etag": {`"33a64df551425fcc55e4d42a148795d9f25f89d4"`}}
	req := http.Header{"If-None-Match": {`"a", "33a64df551425fcc55e4d42a148795d9f25f89d4"`}}
	b.ReportAllocs()
	for b.Loop() {
		NotModified(req, stored)
	}
}
