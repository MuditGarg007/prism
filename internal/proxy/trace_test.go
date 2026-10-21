package proxy

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

const exampleTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestParseTraceparent(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{exampleTraceparent, true},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", true},
		{"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", true},
		{"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-future", true}, // a later version may add fields
		{"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01x", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", false}, // version 00 has exactly four
		{"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", false},
		{"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", false}, // uppercase
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0g", false},
		{"00_4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b-701", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-1", false},
		{"", false},
	}
	for _, tt := range tests {
		id, parent, flags, ok := parseTraceparent(tt.in)
		if ok != tt.ok {
			t.Errorf("parseTraceparent(%q) ok = %v, want %v", tt.in, ok, tt.ok)
		}
		if ok && tt.in == exampleTraceparent {
			if hex.EncodeToString(id[:]) != "4bf92f3577b34da6a3ce929d0e0e4736" || hex.EncodeToString(parent[:]) != "00f067aa0ba902b7" || flags != 1 {
				t.Errorf("parsed %x %x %x", id, parent, flags)
			}
		}
	}
}

// refTraceparent is a regexp version of the spec's rules.
var refTraceparent = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(-.*)?$`)

func refParseTraceparent(s string) bool {
	m := refTraceparent.FindStringSubmatch(s)
	if m == nil || m[1] == "ff" || (m[1] == "00" && m[5] != "") {
		return false
	}
	return strings.Trim(m[2], "0") != "" && strings.Trim(m[3], "0") != ""
}

// FuzzTraceparent checks parseTraceparent against a regexp of the spec's
// rules, and that a parsed version 00 value formats back to itself.
func FuzzTraceparent(f *testing.F) {
	for _, s := range []string{exampleTraceparent, "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-x", "ff-", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		id, parent, flags, ok := parseTraceparent(s)
		if want := refParseTraceparent(s); ok != want {
			t.Fatalf("parseTraceparent(%q) ok = %v, want %v", s, ok, want)
		}
		if ok && strings.HasPrefix(s, "00-") {
			tr := Trace{ID: id, SpanID: parent, Flags: flags}
			if got := tr.traceparent(); got != s {
				t.Fatalf("formatted %q back as %q", s, got)
			}
		}
	})
}

func TestNewTrace(t *testing.T) {
	joined := newTrace(http.Header{traceparentHeader: {exampleTraceparent}})
	if !joined.Joined() || hex.EncodeToString(joined.ID[:]) != "4bf92f3577b34da6a3ce929d0e0e4736" || joined.SpanID == joined.Parent || joined.SpanID == [8]byte{} {
		t.Errorf("joined trace = %+v", joined)
	}
	for _, hdr := range []http.Header{
		{},
		{traceparentHeader: {"garbage"}},
		{traceparentHeader: {exampleTraceparent, exampleTraceparent}}, // ambiguous
	} {
		tr := newTrace(hdr)
		if tr.Joined() || tr.ID == [16]byte{} || tr.ID == joined.ID || tr.SpanID == [8]byte{} || tr.Flags != traceSampled {
			t.Errorf("trace started for %v = %+v", hdr, tr)
		}
	}
}

// TestProxyTrace checks the headers backends get: the client's trace with
// Prism's span as the parent, or a new trace, whose tracestate is not the
// client's. The shadow copy goes out as part of the same trace.
func TestProxyTrace(t *testing.T) {
	var shadowTP atomic.Value
	env := newShadowEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Traceparent", r.Header.Get(traceparentHeader))
		w.Header().Set("X-Got-Tracestate", r.Header.Get(tracestateHeader))
	}, func(w http.ResponseWriter, r *http.Request) {
		shadowTP.Store(r.Header.Get(traceparentHeader))
	})
	tests := []struct {
		name       string
		tp, ts     string
		join       bool
		wantStates string
	}{
		{"joined", exampleTraceparent, "vendor=x", true, "vendor=x"},
		{"none", "", "vendor=x", false, ""},
		{"malformed", "00-zz", "vendor=x", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tt.tp != "" {
				req.Header.Set(traceparentHeader, tt.tp)
			}
			req.Header.Set(tracestateHeader, tt.ts)
			w := &annotatedRecorder{ResponseRecorder: httptest.NewRecorder()}
			env.h.ServeHTTP(w, req)
			got := w.Header().Get("X-Got-Traceparent")
			id, parent, flags, ok := parseTraceparent(got)
			if !ok || !strings.HasPrefix(got, "00-") {
				t.Fatalf("backend got traceparent %q", got)
			}
			an := &w.an
			if id != an.Trace.ID || parent != an.Trace.SpanID || flags != an.Trace.Flags {
				t.Errorf("backend got %q, want trace %x with parent %x", got, an.Trace.ID, an.Trace.SpanID)
			}
			if an.Trace.Joined() != tt.join {
				t.Errorf("joined = %v, want %v", an.Trace.Joined(), tt.join)
			}
			if tt.join && hex.EncodeToString(id[:]) != tt.tp[3:35] {
				t.Errorf("trace ID %x, want the client's", id)
			}
			if st := w.Header().Get("X-Got-Tracestate"); st != tt.wantStates {
				t.Errorf("backend got tracestate %q, want %q", st, tt.wantStates)
			}
			env.result(t)
			if s, _ := shadowTP.Load().(string); s != got {
				t.Errorf("shadow copy got traceparent %q, want %q", s, got)
			}
		})
	}
}

func BenchmarkNewTrace(b *testing.B) {
	for _, tt := range []struct {
		name string
		hdr  http.Header
	}{
		{"join", http.Header{traceparentHeader: {exampleTraceparent}}},
		{"start", http.Header{}},
	} {
		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tr := newTrace(tt.hdr)
				_ = tr.traceparent()
			}
		})
	}
}
