package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/MuditGarg007/prism/internal/upstream"
)

// StatusClientClosedRequest is nginx's non-standard 499: the client went
// away before the response was sent. It is only ever logged, because there
// is nobody left to send it to.
const StatusClientClosedRequest = 499

// errorKind says why a request could not be forwarded. It goes into logs,
// and later into metrics labels, so the values must stay stable.
type errorKind string

const (
	kindClientClosed errorKind = "client_closed"
	kindNoBackend    errorKind = "no_backend"
	kindTimeout      errorKind = "timeout"
	kindConnect      errorKind = "connect"
	kindUpstream     errorKind = "upstream"
)

// classify maps a forwarding error to the status the client gets. ctx is the
// client request's context.
//
// A canceled ctx wins over everything else, because the backend error is then
// only a side effect of the client leaving. A deadline on ctx is not the
// client leaving, so it falls through to 504.
//
// ErrNoBackend is 503 rather than 502: no backend was tried, and the cause
// (every backend down or ejected) is usually temporary, which is what 503
// means. Envoy answers "no healthy upstream" the same way. Timeouts are 504
// whether they hit while connecting or while waiting for headers, as in
// nginx. Everything else, including a refused connection, is 502.
func classify(ctx context.Context, err error) (int, errorKind) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return StatusClientClosedRequest, kindClientClosed
	}
	if errors.Is(err, upstream.ErrNoBackend) {
		return http.StatusServiceUnavailable, kindNoBackend
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return http.StatusGatewayTimeout, kindTimeout
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return http.StatusBadGateway, kindConnect
	}
	return http.StatusBadGateway, kindUpstream
}

// fail answers a request that could not be forwarded, before any of the
// response was written. b is nil when no backend was picked.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, rt *Route, b *upstream.Backend, an *Annotations, err error) {
	status, kind := classify(r.Context(), err)
	an.Error = string(kind)
	level, msg := slog.LevelWarn, "upstream request failed"
	if kind == kindClientClosed {
		level, msg = slog.LevelDebug, "client closed request"
	}
	h.logError(r, level, msg, rt, b, status, kind, err)
	if kind == kindClientClosed {
		return
	}
	http.Error(w, http.StatusText(status), status)
}

// abort ends a response whose body failed to arrive from the backend. The
// headers are already sent, so the status can no longer change; panicking
// with ErrAbortHandler cuts the response off, so the client sees it fail
// instead of mistaking it for a complete one.
func (h *Handler) abort(r *http.Request, rt *Route, b *upstream.Backend, an *Annotations, err error) {
	_, kind := classify(r.Context(), err)
	an.Error = string(kind)
	if kind == kindClientClosed {
		h.logError(r, slog.LevelDebug, "client closed request", rt, b, StatusClientClosedRequest, kind, err)
		return
	}
	h.logError(r, slog.LevelWarn, "upstream response body failed", rt, b, 0, kind, err)
	panic(http.ErrAbortHandler)
}

func (h *Handler) logError(r *http.Request, level slog.Level, msg string, rt *Route, b *upstream.Backend, status int, kind errorKind, err error) {
	ctx := r.Context()
	if !h.log.Enabled(ctx, level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("host", r.Host),
		slog.String("path", r.URL.Path),
		slog.String("upstream", rt.Pool.Name),
	}
	if b != nil {
		attrs = append(attrs, slog.String("backend", b.String()))
	}
	if status != 0 {
		attrs = append(attrs, slog.Int("status", status))
	}
	attrs = append(attrs, slog.String("kind", string(kind)), slog.String("error", err.Error()))
	h.log.LogAttrs(ctx, level, msg, attrs...)
}
