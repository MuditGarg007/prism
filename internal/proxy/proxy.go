package proxy

import (
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
	"github.com/MuditGarg007/prism/internal/ratelimit"
	"github.com/MuditGarg007/prism/internal/resilience"
	"github.com/MuditGarg007/prism/internal/router"
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
	RateLimit    *ratelimit.Limiter // per client IP; nil for none
	MaxBody      int64              // request body limit in bytes; 0 for none
	Concurrency  *limits.Semaphore  // requests served at once; nil for no cap
	Retry        *resilience.Retry  // nil for no retries
}

// Annotations record how the proxy handled one request, for the access log.
type Annotations struct {
	Route    *Route
	Backend  *upstream.Backend
	ClientIP netip.Addr
	Attempts int    // backends tried, counting the first
	Error    string // why forwarding failed, if it did
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
	routes *router.Table[*Route]
	fwd    *Forwarding
	log    *slog.Logger
}

// NewHandler returns a Handler that matches requests against routes and
// reports forwarding failures to log.
func NewHandler(routes *router.Table[*Route], fwd *Forwarding, log *slog.Logger) *Handler {
	return &Handler{routes: routes, fwd: fwd, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	an := &Annotations{}
	if a, ok := w.(Annotated); ok {
		an = a.Annotations()
	}
	an.ClientIP = h.fwd.ClientIP(r.Header, PeerAddr(r.RemoteAddr))

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
	if rt.RateLimit != nil {
		if ok, wait := rt.RateLimit.Allow(an.ClientIP); !ok {
			reject(w, an, http.StatusTooManyRequests, kindRateLimited, wait)
			return
		}
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
	if rt.Concurrency != nil {
		if !rt.Concurrency.TryAcquire() {
			reject(w, an, http.StatusServiceUnavailable, kindOverloaded, time.Second)
			return
		}
		// Held until forward returns, which is after the whole response
		// body is copied: a slow stream is still load on the backend.
		defer rt.Concurrency.Release()
	}
	h.forward(w, r, rt, an)
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

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, rt *Route, an *Annotations) {
	body, err := newRequestBody(r, rt.Retry != nil)
	if err != nil {
		h.clientBodyFailed(w, r, rt, an, err)
		return
	}
	choice := upstream.Choice{ClientIP: an.ClientIP}
	b, err := rt.Pool.Pick(choice)
	if err != nil {
		h.fail(w, r, rt, nil, an, err)
		return
	}
	var resp *http.Response
	for {
		an.Backend = b
		an.Attempts++
		b.Acquire()
		// RoundTrip, not http.Client, so redirects go back to the client.
		resp, err = b.Transport.RoundTrip(h.outgoing(r, rt, b, &body))
		next := h.retryTarget(r, rt, an, &body, &choice, b, resp, err)
		if next == nil {
			break
		}
		if err != nil {
			_, kind := classify(r.Context(), err)
			h.logError(r, slog.LevelWarn, "upstream attempt failed, retrying", rt, b, 0, kind, err)
		} else {
			resp.Body.Close()
		}
		b.Release()
		b = next
	}
	defer b.Release()
	if err != nil {
		h.fail(w, r, rt, b, an, err)
		return
	}
	defer resp.Body.Close()

	removeHopByHop(resp.Header)
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

	readErr, writeErr := copyBody(w, resp.Body, rc)
	if writeErr != nil {
		return // the client went away
	}
	if readErr != nil {
		h.abort(r, rt, b, an, readErr)
	}
	for k, vv := range resp.Trailer {
		wh[http.TrailerPrefix+k] = vv
	}
}

// retryTarget returns the backend to try next after an attempt on b ended
// with resp or err, or nil if the request must not be retried. A retry goes
// only to a backend this request has not tried, so it stops once the pool
// runs out, and the last attempt's result goes to the client as it is.
func (h *Handler) retryTarget(r *http.Request, rt *Route, an *Annotations, body *requestBody, choice *upstream.Choice, b *upstream.Backend, resp *http.Response, err error) *upstream.Backend {
	p := rt.Retry
	if p == nil || an.Attempts > p.Attempts || r.Context().Err() != nil {
		return nil
	}
	var a resilience.Attempt
	if err != nil {
		a.Connect, a.Timeout = failedConnect(err), timedOut(err)
	} else {
		a.Status = resp.StatusCode
	}
	if !p.Retryable(a, resilience.Idempotent(r.Method)) || !body.replayable() {
		return nil
	}
	choice.Tried = append(choice.Tried, b)
	next, err := rt.Pool.Pick(*choice)
	if err != nil {
		return nil
	}
	return next
}

// outgoing builds the request to send to b. Its context is r's, so a client
// that disconnects cancels the backend request too.
func (h *Handler) outgoing(r *http.Request, rt *Route, b *upstream.Backend, rb *requestBody) *http.Request {
	hdr := r.Header.Clone()
	cleanRequestHeaders(hdr)
	h.fwd.Apply(hdr, PeerAddr(r.RemoteAddr), r.Host, r.TLS != nil)
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
