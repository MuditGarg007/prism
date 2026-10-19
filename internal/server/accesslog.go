package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/MuditGarg007/prism/internal/proxy"
)

// logWriter records the status and size of a response for the access log,
// and collects the proxy's annotations.
type logWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	an     proxy.Annotations
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

// accessLog logs one line per request when it finishes, including requests
// whose handler aborted with a panic.
func accessLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &logWriter{ResponseWriter: w}
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
	if !log.Enabled(ctx, slog.LevelInfo) {
		return
	}
	status := lw.status
	if status == 0 {
		// Nothing was written. net/http sends 200 for that, unless the
		// client has already gone.
		status = http.StatusOK
		if ctx.Err() != nil {
			status = proxy.StatusClientClosedRequest
		}
	}
	attrs := make([]slog.Attr, 0, 14)
	attrs = append(attrs,
		slog.String("method", r.Method),
		slog.String("host", r.Host),
		slog.String("path", r.URL.Path),
		slog.String("proto", r.Proto),
		slog.Int("status", status),
		slog.Int64("bytes", lw.bytes),
		slog.Duration("duration", d),
	)
	an := &lw.an
	if an.ClientIP.IsValid() {
		attrs = append(attrs, slog.String("client", an.ClientIP.String()))
	}
	if an.Route != nil {
		attrs = append(attrs, slog.String("route", an.Route.Name), slog.String("upstream", an.Route.Pool.Name))
	}
	if an.Backend != nil {
		attrs = append(attrs, slog.String("backend", an.Backend.String()))
	}
	if an.Error != "" {
		attrs = append(attrs, slog.String("error", an.Error))
	}
	if aborted {
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	log.LogAttrs(ctx, slog.LevelInfo, "request", attrs...)
}
