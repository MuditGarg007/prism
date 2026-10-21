package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MuditGarg007/prism/internal/limits"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/ratelimit"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/router"
	"github.com/MuditGarg007/prism/internal/security"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// Route is what a matched route forwards with. It is read-only once built.
type Route struct {
	Name         string // for logs: the route's host and path prefix
	Pool         *upstream.Pool
	PathPrefix   string
	StripPrefix  bool
	PreserveHost bool
	Request      HeaderOps
	Response     HeaderOps
	RateLimit    *ratelimit.Limiter      // per client IP; nil for none
	MaxBody      int64                   // request body limit in bytes; 0 for none
	Concurrency  *limits.Semaphore       // requests served at once; nil for no cap
	QueueTimeout time.Duration           // longest wait for a Concurrency slot
	Retry        *resilience.Retry       // nil for no retries
	Cache        *Cache                  // nil when the route does not cache
	Access       *security.IPFilter      // checked after matching; nil for none
	Auth         *security.Authenticator // nil for no authentication
	Canary       *Canary                 // nil for no canary
	Shadow       *Shadow                 // nil for no shadow copies
	// WebSocketIdle closes an upgraded connection once nothing has moved
	// either way for this long. 0 means the route does not upgrade.
	WebSocketIdle time.Duration
	// Metrics are the route's metric handles; nil records nothing.
	Metrics *observability.RouteMetrics
}

// Annotations record how the proxy handled one request, for the access log.
type Annotations struct {
	Route *Route
	// Pool is the upstream the request goes to: the route's, or its
	// canary's.
	Pool     *upstream.Pool
	Backend  *upstream.Backend
	ClientIP netip.Addr
	Attempts int    // backends tried, counting the first
	Error    string // why forwarding failed, if it did
	// BudgetExhausted is set when a retry was called for but the route's
	// retry budget refused it.
	BudgetExhausted bool
	// Cache is the X-Cache value sent, or "" when the cache was not used.
	Cache string
	// RequestID is the ID sent to the backend and the client in
	// X-Request-Id.
	RequestID string
	// Trace is the request's W3C trace, sent on to the backend in
	// traceparent.
	Trace Trace
	// User is the user or API key name the route's auth let in, or "".
	User string
	// WebSocket is set when the client asked for a WebSocket on a route
	// that allows them.
	WebSocket bool
	// Upgraded is set once the backend accepted the upgrade. Tunnel is how
	// long the connection then stayed open, and BytesUp and BytesDown are
	// the bytes copied to the backend and to the client.
	Upgraded           bool
	Tunnel             time.Duration
	BytesUp, BytesDown int64
	// Metrics are the handles to record the request with: its route's, or
	// the unmatched ones.
	Metrics *observability.RouteMetrics
}

// Annotated is implemented by a ResponseWriter that wants Annotations
// filled in. The access log uses it, so it needs neither the reloadable
// trusted-proxy list nor a per-request context value.
type Annotated interface {
	Annotations() *Annotations
}

// Handler routes requests and forwards them to backends. It never changes
// after NewHandler, so a config reload swaps in a new Handler instead of
// editing this one.
type Handler struct {
	routes    *router.Table[*Route]
	fwd       *Forwarding
	access    *security.IPFilter
	unmatched *observability.RouteMetrics
	log       *slog.Logger
}

// NewHandler returns a Handler that matches requests against routes and
// reports forwarding failures to log. Clients that access refuses get 403
// before routing; access may be nil. Requests that match no route are
// recorded with unmatched, which may be nil.
func NewHandler(routes *router.Table[*Route], fwd *Forwarding, access *security.IPFilter, unmatched *observability.RouteMetrics, log *slog.Logger) *Handler {
	return &Handler{routes: routes, fwd: fwd, access: access, unmatched: unmatched, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	an := &Annotations{}
	if a, ok := w.(Annotated); ok {
		an = a.Annotations()
	}
	peer := PeerAddr(r.RemoteAddr)
	an.ClientIP = h.fwd.ClientIP(r.Header, peer)
	// Set first, so every response carries it, a rejection included.
	an.RequestID = h.requestID(r.Header, peer)
	w.Header()[RequestIDHeader] = []string{an.RequestID}
	an.Trace = newTrace(r.Header)
	an.Metrics = h.unmatched
	if h.access != nil && !h.access.Allowed(an.ClientIP) {
		reject(w, an, http.StatusForbidden, kindForbidden, 0)
		return
	}

	// The router matches the path as given, and backends may normalize it,
	// so "/public/../admin" would match /public/ and be served as /admin.
	// Rejecting such paths is simpler and safer than normalizing them.
	if !validPath(r.URL.Path) {
		http.Error(w, "invalid request path", http.StatusBadRequest)
		return
	}
	rt, ok := h.routes.Match(r.Host, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	an.Route = rt
	an.Pool = rt.Pool
	an.Metrics = rt.Metrics
	if rt.Access != nil && !rt.Access.Allowed(an.ClientIP) {
		reject(w, an, http.StatusForbidden, kindForbidden, 0)
		return
	}
	if rt.RateLimit != nil {
		if ok, wait := rt.RateLimit.Allow(an.ClientIP); !ok {
			reject(w, an, http.StatusTooManyRequests, kindRateLimited, wait)
			return
		}
	}
	// After the rate limit, so the limit also caps password guessing and
	// the bcrypt work it costs.
	if a := rt.Auth; a != nil {
		user, err := a.Check(r)
		if err != nil {
			if r.Context().Err() != nil {
				h.fail(w, r, rt, nil, an, err) // the client left while waiting
				return
			}
			if c := a.Challenge(); c != "" {
				w.Header().Set("WWW-Authenticate", c)
			}
			reject(w, an, http.StatusUnauthorized, kindUnauthorized, 0)
			return
		}
		an.User = user
	}
	if rt.MaxBody > 0 {
		// A declared length over the limit is refused before reading
		// anything, so a client that sent "Expect: 100-continue" never gets
		// the go-ahead to upload.
		if r.ContentLength > rt.MaxBody {
			closeAfter(w, r)
			reject(w, an, http.StatusRequestEntityTooLarge, kindBodyTooLarge, 0)
			return
		}
		// net/http already stops a declared-length body at its length, so
		// only a body of unknown length needs counting.
		if r.ContentLength < 0 {
			r.Body = http.MaxBytesReader(w, r.Body, rt.MaxBody)
		}
	}
	an.WebSocket = rt.WebSocketIdle > 0 && isWebSocket(r)
	canary := rt.Canary != nil && rt.Canary.pick(r, an.ClientIP)
	if canary {
		an.Pool = rt.Canary.Pool
		an.Metrics = rt.Canary.Metrics
	}
	// The cache comes before the concurrency slot: hits and requests
	// waiting for another's fetch put no load on a backend, so only a
	// request that goes to one takes a slot. The canary skips the cache,
	// so its responses are neither stored for nor served from the
	// primary's. Nor is a WebSocket ever a cached response.
	if c := rt.Cache; c != nil && !canary && !an.WebSocket && cacheable(r) {
		h.serveCache(w, r, rt, c, an)
		return
	}
	if !h.acquire(w, r, rt, an) {
		return
	}
	// Held until forward returns, which is after the whole response body
	// is copied, or a WebSocket closes: a slow stream is still load on the
	// backend.
	defer release(rt)
	h.forward(w, r, rt, an)
}

// acquire takes one of rt's concurrency slots, if it has a limit. If none
// comes free in time it answers the request and returns false. A full queue
// or a wait that times out sheds the request, which pushes back on clients
// instead of letting latency grow.
func (h *Handler) acquire(w http.ResponseWriter, r *http.Request, rt *Route, an *Annotations) bool {
	if rt.Concurrency == nil {
		return true
	}
	if err := rt.Concurrency.Acquire(r.Context(), rt.QueueTimeout); err != nil {
		if r.Context().Err() != nil {
			h.fail(w, r, rt, nil, an, err) // the client left while queued
			return false
		}
		reject(w, an, http.StatusServiceUnavailable, kindOverloaded, time.Second)
		return false
	}
	return true
}

// release gives back the slot acquire took.
func release(rt *Route) {
	if rt.Concurrency != nil {
		rt.Concurrency.Release()
	}
}

// validPath reports whether p is rooted and has no empty, "." or ".."
// segments. A trailing "/" is allowed. p is the decoded path, so "%2e%2e" and
// "%2F" are caught as well.
func validPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	for rest := p[1:]; ; {
		seg, tail, more := strings.Cut(rest, "/")
		if seg == "." || seg == ".." || (seg == "" && more) {
			return false
		}
		if !more {
			return true
		}
		rest = tail
	}
}

// forward sends r to a backend and streams the response back.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, rt *Route, an *Annotations) {
	f, fe := h.fetch(r, rt, an)
	if fe != nil {
		h.fetchFailed(w, r, rt, an, fe)
		return
	}
	defer f.release()
	if f.resp.StatusCode == http.StatusSwitchingProtocols {
		h.tunnel(w, r, rt, an, &f)
		return
	}
	h.write(w, r, rt, an, &f, f.resp.Body)
}

// fetched is a backend's response. The backend stays acquired until
// release.
type fetched struct {
	resp *http.Response
	b    *upstream.Backend
	adm  upstream.Admission
	out  resilience.Outcome
	// upgrade is the protocol a 101 switched to, from its Upgrade header.
	upgrade string
}

func (f *fetched) release() {
	f.resp.Body.Close()
	f.b.Release()
}

// fetchError is why fetch got no response. b is the last backend tried, or
// nil if none was picked.
type fetchError struct {
	b          *upstream.Backend
	err        error
	clientBody bool // reading the client's body failed, so it is the client's fault
}

// fetch sends r to a backend of rt, retrying as the route allows, and
// returns the response with its hop-by-hop headers removed. It writes
// nothing to the client, so the caller decides how to answer either way.
func (h *Handler) fetch(r *http.Request, rt *Route, an *Annotations) (fetched, *fetchError) {
	body, err := newRequestBody(r, rt.Retry != nil)
	if err != nil {
		return fetched{}, &fetchError{err: err, clientBody: true}
	}
	// One copy per request, not per attempt. A body already in memory is
	// copied at once; a streamed one only once this request is done with
	// it. A WebSocket is never copied: there is no response to throw away.
	if !an.WebSocket && shadowFor(r, rt, &body) {
		if body.read == nil {
			h.sendShadow(r, rt, an, &body)
		} else {
			defer h.sendShadow(r, rt, an, &body)
		}
	}
	if p := rt.Retry; p != nil && p.Budget != nil {
		p.Budget.Request()
	}
	choice := upstream.Choice{ClientIP: an.ClientIP}
	b, adm, err := pick(an.Pool, &choice)
	if err != nil {
		return fetched{}, &fetchError{err: err}
	}
	var (
		resp *http.Response
		out  resilience.Outcome
	)
	for {
		an.Backend = b
		an.Attempts++
		b.Acquire()
		start := time.Now()
		// RoundTrip, not http.Client, so redirects go back to the client.
		resp, err = b.Transport.RoundTrip(h.outgoing(r, rt, b, &body, an))
		if err == nil && b.Latency != nil {
			b.Latency.Observe(time.Since(start).Seconds())
		}
		out = outcome(r, &body, resp, err)
		b.Report(adm, out)
		next, nextAdm := h.retryTarget(r, rt, an, &body, &choice, b, resp, err)
		if next == nil {
			break
		}
		if err != nil {
			_, kind := classify(r.Context(), err)
			h.logError(r, slog.LevelWarn, "upstream attempt failed, retrying", an, b, 0, kind, err)
		} else {
			resp.Body.Close()
		}
		b.Release()
		b, adm = next, nextAdm
		// The concurrency slot stays held while backing off, so a route
		// that is retrying a lot takes on less new work.
		if !sleep(r.Context(), rt.Retry.Delay(an.Attempts-1)) {
			b.Report(adm, resilience.Neutral)
			return fetched{}, &fetchError{b: b, err: r.Context().Err()}
		}
	}
	if err != nil {
		b.Release()
		// The transport hands back the client's body read error as its own.
		// Passing max_body is still 413, which fail answers.
		var mbe *http.MaxBytesError
		return fetched{}, &fetchError{b: b, err: err, clientBody: body.clientFailed() && !errors.As(err, &mbe)}
	}
	var upgrade string
	if resp.StatusCode == http.StatusSwitchingProtocols {
		if !an.WebSocket {
			// Nothing but tunnel can answer a 101, and the backend was not
			// asked for one.
			resp.Body.Close()
			b.Overturn(adm)
			b.Release()
			return fetched{}, &fetchError{b: b, err: errBadUpgrade}
		}
		upgrade = resp.Header.Get("Upgrade")
	}
	removeHopByHop(resp.Header)
	// The client gets this request's ID, not one the backend echoes back,
	// and the cache never stores one request's ID to send with another.
	delete(resp.Header, RequestIDHeader)
	return fetched{resp: resp, b: b, adm: adm, out: out, upgrade: upgrade}, nil
}

// fetchFailed answers a request that fetch got no response for.
func (h *Handler) fetchFailed(w http.ResponseWriter, r *http.Request, rt *Route, an *Annotations, fe *fetchError) {
	if fe.clientBody {
		h.clientBodyFailed(w, r, rt, fe.b, an, fe.err)
		return
	}
	h.fail(w, r, rt, fe.b, an, fe.err)
}

// write sends f's response to the client, with the route's response header
// rules applied, reading its body from body: f.resp.Body, or a reader that
// wraps it.
func (h *Handler) write(w http.ResponseWriter, r *http.Request, rt *Route, an *Annotations, f *fetched, body io.Reader) {
	resp := f.resp
	rt.Response.Apply(resp.Header)
	wh := w.Header()
	for k, vv := range resp.Header {
		wh[k] = vv
	}
	if _, ok := resp.Header["Content-Type"]; !ok {
		// A nil value stops net/http from sniffing and adding one.
		wh["Content-Type"] = nil
	}
	if len(resp.Trailer) > 0 {
		wh["Trailer"] = []string{strings.Join(slices.Sorted(maps.Keys(resp.Trailer)), ", ")}
	}
	w.WriteHeader(resp.StatusCode)

	flush := resp.ContentLength == -1 || isEventStream(resp.Header)
	var rc *http.ResponseController
	if flush || len(resp.Trailer) > 0 {
		rc = http.NewResponseController(w)
		// Send the headers now: a stream may not have a body for a while,
		// and announced trailers need a chunked response, which net/http
		// would otherwise replace with a Content-Length for a short body.
		if err := rc.Flush(); errors.Is(err, http.ErrNotSupported) {
			rc = nil
		} else if err != nil {
			return // the client went away
		}
		if !flush {
			rc = nil
		}
	}

	readErr, writeErr := copyBody(w, body, rc)
	if writeErr != nil {
		return // the client went away
	}
	if readErr != nil {
		// The headers counted as a success, but the backend failed to send
		// the body it promised.
		if f.out == resilience.Success && r.Context().Err() == nil {
			f.b.Overturn(f.adm)
		}
		h.abort(r, rt, f.b, an, readErr)
	}
	for k, vv := range resp.Trailer {
		wh[http.TrailerPrefix+k] = vv
	}
}

// pick chooses a backend whose circuit breaker admits the request. A backend
// that refuses, a half-open one with every probe slot taken, is skipped like
// a tried one, so the loop ends once the pool runs out.
func pick(p *upstream.Pool, c *upstream.Choice) (*upstream.Backend, upstream.Admission, error) {
	for {
		b, err := p.Pick(*c)
		if err != nil {
			return nil, upstream.Admission{}, err
		}
		if adm, ok := b.Admit(); ok {
			return b, adm, nil
		}
		c.Tried = append(c.Tried, b)
	}
}

// outcome says what an attempt that ended with resp or err tells about the
// backend's health. Failures the client caused, by leaving or by sending a
// broken body, say nothing about it; otherwise a client could get healthy
// backends ejected.
func outcome(r *http.Request, body *requestBody, resp *http.Response, err error) resilience.Outcome {
	switch {
	case r.Context().Err() != nil || body.clientFailed():
		return resilience.Neutral
	case err != nil || resp.StatusCode >= 500:
		return resilience.Failure
	}
	return resilience.Success
}

// retryTarget returns the backend to try next after an attempt on b ended
// with resp or err, and its admission, or nil if the request must not be
// retried. A retry goes
// only to a backend this request has not tried, so it stops once the pool
// runs out, and the last attempt's result goes to the client as it is.
func (h *Handler) retryTarget(r *http.Request, rt *Route, an *Annotations, body *requestBody, choice *upstream.Choice, b *upstream.Backend, resp *http.Response, err error) (*upstream.Backend, upstream.Admission) {
	p := rt.Retry
	if p == nil || an.Attempts > p.Attempts || r.Context().Err() != nil {
		return nil, upstream.Admission{}
	}
	var a resilience.Attempt
	if err != nil {
		a.Connect, a.Timeout = failedConnect(err), timedOut(err)
	} else {
		a.Status = resp.StatusCode
	}
	if !p.Retryable(a, resilience.Idempotent(r.Method)) || !body.replayable() {
		return nil, upstream.Admission{}
	}
	if p.Budget != nil && !p.Budget.TryRetry() {
		an.BudgetExhausted = true
		return nil, upstream.Admission{}
	}
	choice.Tried = append(choice.Tried, b)
	next, adm, err := pick(an.Pool, choice)
	if err != nil {
		return nil, upstream.Admission{}
	}
	return next, adm
}

// sleep waits for d, or until ctx is done, and reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// outgoing builds the request to send to b for r, which an describes. Its
// context is r's, so a client that disconnects cancels the backend request
// too.
func (h *Handler) outgoing(r *http.Request, rt *Route, b *upstream.Backend, rb *requestBody, an *Annotations) *http.Request {
	hdr := r.Header.Clone()
	cleanRequestHeaders(hdr)
	if an.WebSocket {
		// The one hop-by-hop request Prism passes on, by asking for it again.
		hdr["Connection"] = []string{"Upgrade"}
		hdr["Upgrade"] = []string{"websocket"}
	}
	h.fwd.Apply(hdr, PeerAddr(r.RemoteAddr), r.Host, r.TLS != nil)
	// One array for both values, each capped so an append to one copies it.
	ids := []string{an.RequestID, an.Trace.traceparent()}
	hdr[RequestIDHeader] = ids[0:1:1]
	an.Trace.propagate(hdr, ids[1:2:2])
	if rt.Auth != nil {
		rt.Auth.Prepare(hdr, an.User)
	}
	rt.Request.Apply(hdr)

	host := b.URL.Host
	if rt.PreserveHost {
		host = r.Host
	}
	// Go sends Request.Host and ignores Header["Host"], so a Host rule has to
	// be moved across.
	if v, ok := rt.Request.Get("Host"); ok {
		host = v
		delete(hdr, "Host")
	}
	if _, ok := hdr["User-Agent"]; !ok {
		// An empty value stops the transport from adding its own.
		hdr["User-Agent"] = []string{""}
	}

	body, getBody := rb.attempt()
	out := &http.Request{
		Method:        r.Method,
		URL:           targetURL(b.URL, r.URL, rt.PathPrefix, rt.StripPrefix),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          body,
		GetBody:       getBody,
		ContentLength: r.ContentLength,
		Trailer:       r.Trailer,
		Host:          host,
	}
	return out.WithContext(r.Context())
}

// targetURL points in at backend, stripping prefix from the path when strip
// is set. Backends have no base path (validation forbids one), so the path
// is used as is.
//
// RawPath keeps escapes such as "%2F" that Path cannot express. It is
// stripped only when it starts with the same prefix; otherwise it is dropped
// and the transport encodes Path instead.
func targetURL(backend, in *url.URL, prefix string, strip bool) *url.URL {
	p, raw := in.Path, in.RawPath
	if strip {
		p = router.StripPrefix(p, prefix)
		if strings.HasPrefix(raw, prefix) {
			raw = router.StripPrefix(raw, prefix)
		} else {
			raw = ""
		}
	}
	return &url.URL{
		Scheme:     backend.Scheme,
		Host:       backend.Host,
		Path:       p,
		RawPath:    raw,
		RawQuery:   in.RawQuery,
		ForceQuery: in.ForceQuery,
	}
}

func isEventStream(h http.Header) bool {
	mt, _, _ := strings.Cut(h.Get("Content-Type"), ";")
	return strings.EqualFold(textproto.TrimString(mt), "text/event-stream")
}

// bufPool holds the 32 KB buffers that response bodies are copied through.
// Pointers avoid an allocation on every Put.
var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 32<<10)
	return &b
}}

// copyBody streams src to w, flushing after every write when rc is set and
// rc can flush. It
// tells a failed read from the backend apart from a failed write to the
// client, because only the first one is the backend's fault.
func copyBody(w io.Writer, src io.Reader, rc *http.ResponseController) (readErr, writeErr error) {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil, werr
			}
			if rc != nil {
				if ferr := rc.Flush(); errors.Is(ferr, http.ErrNotSupported) {
					rc = nil
				} else if ferr != nil {
					return nil, ferr
				}
			}
		}
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return err, nil
		}
	}
}
