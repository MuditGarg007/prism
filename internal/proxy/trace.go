package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// The W3C Trace Context headers (https://www.w3.org/TR/trace-context/), in
// canonical form.
const (
	traceparentHeader = "Traceparent"
	tracestateHeader  = "Tracestate"
)

// traceSampled is the sampled bit of the trace flags.
const traceSampled = 0x01

// Trace is a request's place in a W3C trace. Prism counts as one span of
// it: SpanID, a child of Parent. Every request Prism sends for it, retries
// and the shadow copy included, names SpanID as its parent, so the parent a
// backend sees is the span_id in Prism's access log.
type Trace struct {
	ID     [16]byte
	Parent [8]byte // the client's span; zero when Prism started the trace
	SpanID [8]byte
	Flags  byte
}

// Joined reports whether the trace came from the client rather than being
// started by Prism.
func (t *Trace) Joined() bool { return t.Parent != [8]byte{} }

// newTrace continues the trace in hdr's traceparent, or starts a new one
// when there is none or it is malformed. A started trace is sampled, as
// OpenTelemetry's default sampler samples a root span: whether to record it
// is then up to each backend's own sampler.
//
// The client's traceparent is accepted whoever sends it. It decides
// nothing in Prism, and joining a trace started in a browser or an app is
// the point of propagating it.
func newTrace(hdr http.Header) Trace {
	var t Trace
	if vv := hdr[traceparentHeader]; len(vv) == 1 {
		if id, parent, flags, ok := parseTraceparent(vv[0]); ok {
			t.ID, t.Parent, t.Flags = id, parent, flags
			rand.Read(t.SpanID[:])
			return t
		}
	}
	var b [24]byte
	rand.Read(b[:]) // never fails
	copy(t.ID[:], b[:16])
	copy(t.SpanID[:], b[16:])
	t.Flags = traceSampled
	return t
}

// traceparent formats the header for a request Prism sends as part of t.
// Prism speaks version 00, whatever version the client sent.
func (t *Trace) traceparent() string {
	var buf [55]byte
	b := append(buf[:0], "00-"...)
	b = hex.AppendEncode(b, t.ID[:])
	b = append(b, '-')
	b = hex.AppendEncode(b, t.SpanID[:])
	b = append(b, '-')
	b = hex.AppendEncode(b, []byte{t.Flags})
	return string(b)
}

// propagate sets the trace headers on hdr, a request going to a backend.
// The client's tracestate goes on only with the trace it belongs to.
func (t *Trace) propagate(hdr http.Header) {
	hdr[traceparentHeader] = []string{t.traceparent()}
	if !t.Joined() {
		delete(hdr, tracestateHeader)
	}
}

// parseTraceparent parses a traceparent value: version, trace ID, parent
// ID and flags, as lowercase hex separated by dashes. A version other than
// 00 may append fields, which are ignored; version ff and all-zero IDs are
// invalid.
func parseTraceparent(s string) (id [16]byte, parent [8]byte, flags byte, ok bool) {
	const size = 55 // "00-" + 32 + "-" + 16 + "-" + 2
	if len(s) < size || s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return id, parent, 0, false
	}
	var version [1]byte
	if !unhexLower(version[:], s[0:2]) || version[0] == 0xff {
		return id, parent, 0, false
	}
	if len(s) > size && (version[0] == 0 || s[size] != '-') {
		return id, parent, 0, false
	}
	var f [1]byte
	if !unhexLower(id[:], s[3:35]) || !unhexLower(parent[:], s[36:52]) || !unhexLower(f[:], s[53:55]) {
		return id, parent, 0, false
	}
	if id == [16]byte{} || parent == [8]byte{} {
		return id, parent, 0, false
	}
	return id, parent, f[0], true
}

// unhexLower decodes s, of len(dst)*2 lowercase hex digits, into dst. The
// spec allows no uppercase, unlike hex.Decode.
func unhexLower(dst []byte, s string) bool {
	for i := range dst {
		hi, ok1 := lowerHexDigit(s[2*i])
		lo, ok2 := lowerHexDigit(s[2*i+1])
		if !ok1 || !ok2 {
			return false
		}
		dst[i] = hi<<4 | lo
	}
	return true
}

func lowerHexDigit(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}
