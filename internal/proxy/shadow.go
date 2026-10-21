package proxy

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/MuditGarg007/prism/internal/limits"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// ShadowHeader marks the copies Shadow sends, so their backend can tell
// them apart from real traffic, for example to skip side effects.
const ShadowHeader = "X-Prism-Shadow"

// Shadow sends copies of a route's requests to another upstream and throws
// the responses away.
//
// A copy never delays the request it copies. It goes out on its own
// goroutine, under its own timeout, and is not canceled when the client
// leaves. Its body is one the request already had in memory, or one
// captured while the request streamed it to its own backend; a body that
// would have to be waited for is not copied. When Slots are all taken the
// copy is dropped rather than queued.
type Shadow struct {
	Pool *upstream.Pool
	// Buckets is how many of 10,000 requests are copied: percent × 100.
	Buckets uint32
	MaxBody int64
	Slots   *limits.Semaphore // copies in flight
	Timeout time.Duration     // for a whole copy, its response body included
	Metrics *observability.ShadowMetrics

	onDone func(observability.ShadowResult) // for tests; nil in use
}

func (s *Shadow) sample() bool {
	return s.Buckets >= canaryBuckets || rand.Uint32N(canaryBuckets) < s.Buckets
}

func (s *Shadow) done(r observability.ShadowResult) {
	s.Metrics.Inc(r)
	if s.onDone != nil {
		s.onDone(r)
	}
}

// shadowFor decides whether r is copied, and if so prepares body so that it
// keeps a copy of what it streams. It returns false for a request that is
// not copied; one that was sampled but has an unsuitable body is counted
// here.
func shadowFor(r *http.Request, rt *Route, body *requestBody) bool {
	s := rt.Shadow
	if s == nil || !s.sample() {
		return false
	}
	if r.ContentLength < 0 || r.ContentLength > s.MaxBody {
		s.done(observability.ShadowSkipped)
		return false
	}
	if body.read != nil {
		body.read.capture(r.ContentLength)
	}
	return true
}

// sendShadow sends the copy of r that shadowFor prepared. For a streamed
// body it must be called once the request's own attempts are over: a body
// the backend had not finished reading by then is not copied, since
// waiting for it would hold up the response.
func (h *Handler) sendShadow(r *http.Request, rt *Route, an *Annotations, body *requestBody) {
	s := rt.Shadow
	sb := requestBody{empty: body.empty, buf: body.buf}
	if body.read != nil {
		p := body.read.captured.Load()
		if p == nil {
			s.done(observability.ShadowSkipped)
			return
		}
		sb.buf = *p
	}
	if !s.Slots.TryAcquire() {
		s.done(observability.ShadowBusy)
		return
	}
	b, adm, err := pick(s.Pool, &upstream.Choice{ClientIP: an.ClientIP})
	if err != nil {
		s.Slots.Release()
		s.done(observability.ShadowError)
		return
	}
	// Built here, not on the copy's goroutine: r must not be read once its
	// handler has returned.
	req := h.outgoing(r, rt, b, &sb, an)
	req.Header[ShadowHeader] = []string{"1"}
	req.Trailer = nil // r's, which net/http fills in as it reads r's body
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.Timeout)
	req = req.WithContext(ctx)
	b.Acquire()
	go func() {
		defer s.Slots.Release()
		defer cancel()
		defer b.Release()
		s.done(roundTripShadow(req, b, adm))
	}()
}

// roundTripShadow sends req to b and reads the whole response, so the
// connection can be reused, then reports to b how it went.
func roundTripShadow(req *http.Request, b *upstream.Backend, adm upstream.Admission) observability.ShadowResult {
	start := time.Now()
	resp, err := b.Transport.RoundTrip(req)
	if err != nil {
		b.Report(adm, resilience.Failure)
		return observability.ShadowError
	}
	if b.Latency != nil {
		b.Latency.Observe(time.Since(start).Seconds())
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode >= 500 {
		b.Report(adm, resilience.Failure)
	} else {
		b.Report(adm, resilience.Success)
	}
	return observability.ShadowSent
}

// shadowCapture keeps a copy of a streamed body for a shadow request. Only
// the transport's goroutine writes buf. Once all want bytes have arrived it
// publishes buf, after which nothing writes to it again.
type shadowCapture struct {
	buf  []byte
	want int64
}

// capture makes c keep a copy of the next want bytes it reads.
func (c *readCounter) capture(want int64) {
	c.shadow = &shadowCapture{buf: make([]byte, 0, want), want: want}
}

// keep copies p, just read, into the capture, and publishes it once
// complete.
func (c *readCounter) keep(p []byte) {
	s := c.shadow
	if s == nil || int64(len(s.buf)) >= s.want {
		return
	}
	s.buf = append(s.buf, p[:min(int64(len(p)), s.want-int64(len(s.buf)))]...)
	if int64(len(s.buf)) == s.want {
		c.captured.Store(&s.buf)
	}
}
