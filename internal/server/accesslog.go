package server

import (
	"bufio"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/proxy"
)

// logWriter records the status and size of a response for the access log,
// and collects the proxy's annotations.
type logWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	an     proxy.Annotations
	track  func(net.Conn) (untrack func()) // nil to track nothing
}

func (w *logWriter) WriteHeader(code int) {
	// 1xx responses may come first; the final status is the one to log.
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, so Flush and
// Hijack still work through the access log.
func (w *logWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *logWriter) Annotations() *proxy.Annotations { return &w.an }

// HijackTracked hands the connection over for a WebSocket, registered with
// the server until done is called.
func (w *logWriter) HijackTracked() (net.Conn, *bufio.ReadWriter, func(), error) {
	c, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, nil, err
	}
	// Only the proxy's own 101 can be written from here on.
	w.status = http.StatusSwitchingProtocols
	done := func() {}
	if w.track != nil {
		done = w.track(c)
	}
	return c, brw, done, nil
}

// accessLog logs one line per request when it finishes, and records it in
// the metrics the proxy annotated it with, including requests whose handler
// aborted with a panic. Connections hijacked for WebSockets are registered
// with track, which may be nil.
func accessLog(next http.Handler, log *slog.Logger, track func(net.Conn) func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &logWriter{ResponseWriter: w, track: track}
		start := time.Now()
		defer func() {
			p := recover()
			logRequest(log, r, lw, time.Since(start), p != nil)
			if p != nil {
				panic(p) // keep net/http's handling, such as ErrAbortHandler
			}
		}()
		next.ServeHTTP(lw, r)
	})
}

func logRequest(log *slog.Logger, r *http.Request, lw *logWriter, d time.Duration, aborted bool) {
	ctx := r.Context()
	status := lw.status
	if status == 0 {
		// Nothing was written. net/http sends 200 for that, unless the
		// client has already gone.
		status = http.StatusOK
		if ctx.Err() != nil {
			status = proxy.StatusClientClosedRequest
		}
	}
	an := &lw.an
	an.Metrics.Observe(observability.Request{
		Status: status,
		// A WebSocket's lifetime would swamp the latency histogram, so only
		// its handshake counts.
		Duration:        d - an.Tunnel,
		Attempts:        an.Attempts,
		BudgetExhausted: an.BudgetExhausted,
		ErrorKind:       an.Error,
	})
	if !log.Enabled(ctx, slog.LevelInfo) {
		return
	}
	attrs := make([]slog.Attr, 0, 16)
	if an.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", an.RequestID))
	}
	if t := &an.Trace; t.ID != [16]byte{} {
		attrs = append(attrs, slog.String("trace_id", hex.EncodeToString(t.ID[:])), slog.String("span_id", hex.EncodeToString(t.SpanID[:])))
		if t.Joined() {
			attrs = append(attrs, slog.String("parent_id", hex.EncodeToString(t.Parent[:])))
		}
	}
	attrs = append(attrs,
		slog.String("method", r.Method),
		slog.String("host", r.Host),
		slog.String("path", r.URL.Path),
		slog.String("proto", r.Proto),
		slog.Int("status", status),
		slog.Int64("bytes", lw.bytes),
		slog.Duration("duration", d),
	)
	if an.ClientIP.IsValid() {
		attrs = append(attrs, slog.String("client", an.ClientIP.String()))
	}
	if an.User != "" {
		attrs = append(attrs, slog.String("user", an.User))
	}
	if an.Route != nil {
		attrs = append(attrs, slog.String("route", an.Route.Name), slog.String("upstream", an.Pool.Name))
	}
	if an.Backend != nil {
		attrs = append(attrs, slog.String("backend", an.Backend.String()), slog.Int("attempts", an.Attempts))
	}
	if an.Cache != "" {
		attrs = append(attrs, slog.String("cache", an.Cache))
	}
	if an.Upgraded {
		attrs = append(attrs, slog.Bool("websocket", true), slog.Int64("bytes_up", an.BytesUp), slog.Int64("bytes_down", an.BytesDown))
	}
	if an.Error != "" {
		attrs = append(attrs, slog.String("error", an.Error))
	}
	if an.BudgetExhausted {
		attrs = append(attrs, slog.Bool("retry_budget_exhausted", true))
	}
	if aborted {
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	log.LogAttrs(ctx, slog.LevelInfo, "request", attrs...)
}
