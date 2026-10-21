package proxy

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errBadUpgrade is a 101 from a backend that was not asked for one, or that
// switched to a protocol other than WebSocket. It is the backend's fault.
var errBadUpgrade = errors.New("backend switched protocols without a WebSocket upgrade")

// isWebSocket reports whether r asks to upgrade to a WebSocket (RFC 6455,
// section 4.1). Only HTTP/1.1 can upgrade: HTTP/2 has no Connection header.
func isWebSocket(r *http.Request) bool {
	return r.Method == http.MethodGet && r.ProtoMajor == 1 && r.ProtoMinor >= 1 &&
		hasToken(r.Header["Connection"], "upgrade") && hasToken(r.Header["Upgrade"], "websocket")
}

// Hijacker is implemented by a ResponseWriter that hands over its
// connection for a WebSocket and tracks it until done is called, so a
// shutdown waits for the connection and closes it at the deadline. The
// access log's writer implements it; any other writer is hijacked
// untracked.
type Hijacker interface {
	HijackTracked() (c net.Conn, brw *bufio.ReadWriter, done func(), err error)
}

func hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, func(), error) {
	if h, ok := w.(Hijacker); ok {
		return h.HijackTracked()
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	return c, brw, func() {}, err
}

// tunnel completes the WebSocket upgrade that f's backend accepted with a
// 101, then copies bytes both ways until either side closes or nothing has
// moved in either direction for the route's idle timeout. The caller still
// holds the concurrency slot and the backend, so both count the connection
// for as long as it is open.
func (h *Handler) tunnel(w http.ResponseWriter, r *http.Request, rt *Route, an *Annotations, f *fetched) {
	back, ok := f.resp.Body.(io.ReadWriteCloser)
	if !ok || !strings.EqualFold(f.upgrade, "websocket") {
		f.b.Overturn(f.adm)
		h.fail(w, r, rt, f.b, an, errBadUpgrade)
		return
	}
	conn, brw, done, err := hijack(w)
	if err != nil {
		an.Error = string(kindInternal)
		h.logError(r, slog.LevelError, "hijacking the client connection failed", an, f.b, http.StatusInternalServerError, kindInternal, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	defer done()
	defer conn.Close()

	// Written by hand, as httputil.ReverseProxy does: net/http treats a 101
	// passed to WriteHeader as a final response with a body.
	resp := f.resp
	rt.Response.Apply(resp.Header)
	wh := w.Header() // the request ID, set before routing
	for k, vv := range resp.Header {
		wh[k] = vv
	}
	wh["Connection"] = []string{"Upgrade"}
	wh["Upgrade"] = []string{"websocket"}
	bw := brw.Writer
	bw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	wh.Write(bw)
	bw.WriteString("\r\n")
	if err := bw.Flush(); err != nil {
		return // the client went away
	}
	an.Upgraded = true
	start := time.Now()

	// Frames the client sent right behind its request are already buffered.
	var client io.Reader = conn
	if n := brw.Reader.Buffered(); n > 0 {
		client = io.MultiReader(io.LimitReader(brw.Reader, int64(n)), conn)
	}
	closeBoth := func() {
		conn.Close()
		back.Close()
	}
	idle := newIdleTimer(rt.WebSocketIdle, closeBoth)
	var (
		wg sync.WaitGroup
		up int64
	)
	// Either direction ending ends both, as in httputil.ReverseProxy: the
	// WebSocket close handshake has happened by then, or never will.
	wg.Go(func() {
		up = pipe(back, client, idle)
		closeBoth()
	})
	down := pipe(conn, back, idle)
	closeBoth()
	wg.Wait()
	idle.stop()
	an.BytesUp, an.BytesDown = up, down
	an.Tunnel = time.Since(start)
}

// pipe copies src to dst until either fails, touching idle after every
// read, and returns how many bytes it wrote.
func pipe(dst io.Writer, src io.Reader, idle *idleTimer) int64 {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	var n int64
	for {
		nr, err := src.Read(buf)
		if nr > 0 {
			idle.touch()
			nw, werr := dst.Write(buf[:nr])
			n += int64(nw)
			if werr != nil {
				return n
			}
		}
		if err != nil {
			return n
		}
	}
}

// idleTimer calls expire once d has passed without a touch. A touch only
// stores the time, so the hot path takes no lock and resets no timer; the
// timer checks the time when it fires and goes back to sleep for the rest.
type idleTimer struct {
	d      time.Duration
	start  time.Time
	last   atomic.Int64 // when last touched, as nanoseconds since start
	expire func()

	mu      sync.Mutex // guards t, which check may read before it is set
	t       *time.Timer
	stopped bool
}

func newIdleTimer(d time.Duration, expire func()) *idleTimer {
	it := &idleTimer{d: d, start: time.Now(), expire: expire}
	it.mu.Lock()
	defer it.mu.Unlock()
	it.t = time.AfterFunc(d, it.check)
	return it
}

func (it *idleTimer) touch() { it.last.Store(int64(time.Since(it.start))) }

func (it *idleTimer) check() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.stopped {
		return
	}
	idle := time.Since(it.start) - time.Duration(it.last.Load())
	if idle >= it.d {
		it.stopped = true
		it.expire()
		return
	}
	it.t.Reset(it.d - idle)
}

func (it *idleTimer) stop() {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.stopped = true
	it.t.Stop()
}
