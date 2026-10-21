package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/cache"
	"github.com/MuditGarg007/prism/internal/limits"
)

// wsBackend answers every request that asks for an upgrade, and any request
// at all when force is set, with a 101 to protocol, then hands the
// connection to onConn. Other requests get "plain". The headers of the last
// request are kept in last.
type wsBackend struct {
	srv  *httptest.Server
	last atomic.Pointer[http.Header]
	done chan struct{} // gets a value whenever onConn returns
}

func newWSBackend(t *testing.T, protocol string, force bool, onConn func(net.Conn, *bufio.ReadWriter)) *wsBackend {
	t.Helper()
	b := &wsBackend{done: make(chan struct{}, 16)}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		b.last.Store(&h)
		if r.Header.Get("Upgrade") == "" && !force {
			io.WriteString(w, "plain")
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		io.WriteString(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+protocol+
			"\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: accepted\r\nX-Request-Id: from-backend\r\n\r\n")
		brw.Flush()
		onConn(c, brw)
		b.done <- struct{}{}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func echo(c net.Conn, brw *bufio.ReadWriter) { io.Copy(c, brw) }

// wsFront serves h through a writer that collects Annotations and tracks
// hijacked connections, as the access log's does. Each request's
// Annotations arrive on the channel once its handler returns.
type wsFront struct {
	url     string
	an      chan *Annotations
	tracked atomic.Int64 // hijacked connections not yet done
}

type trackingWriter struct {
	http.ResponseWriter
	an    Annotations
	front *wsFront
}

func (w *trackingWriter) Annotations() *Annotations { return &w.an }

// Unwrap lets http.ResponseController flush, as through the access log.
func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingWriter) HijackTracked() (net.Conn, *bufio.ReadWriter, func(), error) {
	c, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, nil, err
	}
	w.front.tracked.Add(1)
	return c, brw, func() { w.front.tracked.Add(-1) }, nil
}

func newWSFront(t *testing.T, h http.Handler) *wsFront {
	f, srv := newTrackingFront(h)
	srv.Start()
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// newTrackingFront returns an unstarted server for h, which serves through
// a trackingWriter.
func newTrackingFront(h http.Handler) (*wsFront, *httptest.Server) {
	f := &wsFront{an: make(chan *Annotations, 16)}
	return f, httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w, front: f}
		h.ServeHTTP(tw, r)
		f.an <- &tw.an
	}))
}

func (f *wsFront) annotations(t *testing.T) *Annotations {
	t.Helper()
	select {
	case an := <-f.an:
		return an
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
		return nil
	}
}

// dialWS sends an upgrade request for path to the server at url, followed
// at once by early, and returns the connection and the response.
func dialWS(t *testing.T, url, path, early string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: example.com\r\nConnection: keep-alive, Upgrade\r\n"+
		"Upgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"+early)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp
}

// roundTrip writes msg and reads back as many bytes.
func roundTrip(t *testing.T, c net.Conn, br *bufio.Reader, msg string) {
	t.Helper()
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	readN(t, br, msg)
}

func readN(t *testing.T, br *bufio.Reader, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("reading %q: %v", want, err)
	}
	if string(got) != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

// waitClosed waits for the server to close c, and returns how long that
// took.
func waitClosed(t *testing.T, br *bufio.Reader) time.Duration {
	t.Helper()
	start := time.Now()
	if n, err := br.Read(make([]byte, 1)); err == nil {
		t.Fatalf("read %d bytes, want the connection closed", n)
	}
	return time.Since(start)
}

func TestProxyWebSocketEcho(t *testing.T) {
	backend := newWSBackend(t, "websocket", false, echo)
	rt := &Route{
		WebSocketIdle: time.Minute,
		Response:      NewHeaderOps(map[string]string{"X-Route": "ws"}, nil),
	}
	front := newWSFront(t, newHandler(t, testUpstream(backend.srv.URL), rt, slog.New(slog.DiscardHandler)))

	c, br, resp := dialWS(t, front.url, "/chat", "early")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	for name, want := range map[string]string{
		"Upgrade":              "websocket",
		"Connection":           "Upgrade",
		"Sec-Websocket-Accept": "accepted",
		"X-Route":              "ws",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("response %s = %q, want %q", name, got, want)
		}
	}
	if id := resp.Header.Get(RequestIDHeader); id == "" || id == "from-backend" {
		t.Errorf("response X-Request-Id = %q, want the proxy's", id)
	}
	seen := *backend.last.Load()
	for name, want := range map[string]string{
		"Upgrade":               "websocket",
		"Connection":            "Upgrade",
		"Sec-Websocket-Key":     "dGhlIHNhbXBsZSBub25jZQ==",
		"Sec-Websocket-Version": "13",
	} {
		if got := seen.Get(name); got != want {
			t.Errorf("backend got %s = %q, want %q", name, got, want)
		}
	}

	readN(t, br, "early") // sent along with the request, before the 101
	roundTrip(t, c, br, "hello")
	roundTrip(t, c, br, strings.Repeat("x", 100<<10))
	if n := front.tracked.Load(); n != 1 {
		t.Errorf("%d connections tracked, want 1", n)
	}
	if n := rt.Pool.Backends[0].Inflight(); n != 1 {
		t.Errorf("backend inflight = %d while open, want 1", n)
	}
	c.Close()

	an := front.annotations(t)
	want := int64(len("early") + len("hello") + 100<<10)
	if !an.WebSocket || !an.Upgraded || an.BytesUp != want || an.BytesDown != want || an.Tunnel <= 0 {
		t.Errorf("annotations: websocket %v, upgraded %v, up %d, down %d, tunnel %v; want both %d",
			an.WebSocket, an.Upgraded, an.BytesUp, an.BytesDown, an.Tunnel, want)
	}
	if n := front.tracked.Load(); n != 0 {
		t.Errorf("%d connections still tracked", n)
	}
	select {
	case <-backend.done:
	case <-time.After(5 * time.Second):
		t.Fatal("backend connection not closed")
	}
	if n := an.Backend.Inflight(); n != 0 {
		t.Errorf("backend inflight = %d after close, want 0", n)
	}
}

func TestProxyWebSocketIdle(t *testing.T) {
	const idle = 100 * time.Millisecond
	t.Run("client traffic", func(t *testing.T) {
		backend := newWSBackend(t, "websocket", false, echo)
		front := newWSFront(t, newHandler(t, testUpstream(backend.srv.URL), &Route{WebSocketIdle: idle}, slog.New(slog.DiscardHandler)))
		c, br, _ := dialWS(t, front.url, "/", "")
		for range 8 { // 320 ms, over three idle timeouts
			time.Sleep(idle * 2 / 5)
			roundTrip(t, c, br, "ping")
		}
		if d := waitClosed(t, br); d < idle*9/10 {
			t.Errorf("closed %v after the last message, want at least %v", d, idle)
		}
		front.annotations(t)
	})
	t.Run("backend traffic", func(t *testing.T) {
		backend := newWSBackend(t, "websocket", false, func(c net.Conn, brw *bufio.ReadWriter) {
			for range 8 {
				time.Sleep(idle * 2 / 5)
				if _, err := io.WriteString(c, "tick"); err != nil {
					return
				}
			}
			io.Copy(io.Discard, brw) // silent until the proxy closes
		})
		front := newWSFront(t, newHandler(t, testUpstream(backend.srv.URL), &Route{WebSocketIdle: idle}, slog.New(slog.DiscardHandler)))
		_, br, _ := dialWS(t, front.url, "/", "")
		for range 8 {
			readN(t, br, "tick")
		}
		if d := waitClosed(t, br); d < idle*9/10 {
			t.Errorf("closed %v after the last tick, want at least %v", d, idle)
		}
		select {
		case <-backend.done:
		case <-time.After(5 * time.Second):
			t.Fatal("backend connection not closed")
		}
		front.annotations(t)
	})
}

func TestProxyWebSocketRefused(t *testing.T) {
	// Without websocket on the route, the upgrade headers are dropped like
	// any hop-by-hop header, so the backend answers a plain request.
	backend := newWSBackend(t, "websocket", false, echo)
	url := serve(t, newHandler(t, testUpstream(backend.srv.URL), &Route{}, slog.New(slog.DiscardHandler)))
	_, br, resp := dialWS(t, url, "/", "")
	body, _ := io.ReadAll(io.LimitReader(br, 5))
	if resp.StatusCode != http.StatusOK || string(body) != "plain" {
		t.Errorf("got %d %q, want 200 plain", resp.StatusCode, body)
	}
	if h := *backend.last.Load(); h.Get("Upgrade") != "" || h.Get("Connection") != "" {
		t.Errorf("backend got Upgrade %q, Connection %q", h.Get("Upgrade"), h.Get("Connection"))
	}

	tests := []struct {
		name     string
		protocol string
		force    bool
		ws       bool
	}{
		{"101 not asked for", "websocket", true, false},
		{"other protocol", "h2c", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newWSBackend(t, tt.protocol, tt.force, echo)
			rt := &Route{}
			if tt.ws {
				rt.WebSocketIdle = time.Minute
			}
			front := newWSFront(t, newHandler(t, testUpstream(backend.srv.URL), rt, slog.New(slog.DiscardHandler)))
			_, _, resp := dialWS(t, front.url, "/", "")
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status %d, want 502", resp.StatusCode)
			}
			if an := front.annotations(t); an.Upgraded || an.Error != "upstream" {
				t.Errorf("upgraded %v, error %q; want upstream", an.Upgraded, an.Error)
			}
		})
	}
}

// TestProxyWebSocketHoldsSlot checks that an open WebSocket keeps its
// route's concurrency slot, and that neither the cache nor the shadow sees
// it.
func TestProxyWebSocketHoldsSlot(t *testing.T) {
	sem := limits.NewSemaphore(1, 0)
	rt := &Route{WebSocketIdle: time.Minute, Concurrency: sem}
	env := newShadowEnv(t, rt, func(w http.ResponseWriter, r *http.Request) {
		c, brw, _ := http.NewResponseController(w).Hijack()
		defer c.Close()
		io.WriteString(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		echo(c, brw)
	}, func(w http.ResponseWriter, r *http.Request) {})
	rt.Cache = newCacheEnv(t, nil, fixed("public, max-age=60", "cached")).c

	c, br, resp := dialWS(t, env.front, "/", "")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	roundTrip(t, c, br, "hi")
	req, _ := http.NewRequest("POST", env.front+"/", strings.NewReader("x"))
	if resp, _ := get(t, req); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second request got %d while the WebSocket is open, want 503", resp.StatusCode)
	}
	c.Close()
	waitIdle(t, sem)
	if st := rt.Cache.Stats.Snapshot(); st != (cache.StatsSnapshot{}) {
		t.Errorf("cache was used: %+v", st)
	}
	select {
	case s := <-env.seen:
		t.Errorf("shadow got a copy: %+v", s)
	default:
	}
}

func TestIsWebSocket(t *testing.T) {
	tests := []struct {
		method, proto, conn, upgrade string
		want                         bool
	}{
		{"GET", "HTTP/1.1", "Upgrade", "websocket", true},
		{"GET", "HTTP/1.1", "keep-alive, upgrade", "WebSocket", true},
		{"GET", "HTTP/1.1", "keep-alive", "websocket", false},
		{"GET", "HTTP/1.1", "Upgrade", "h2c", false},
		{"POST", "HTTP/1.1", "Upgrade", "websocket", false},
		{"GET", "HTTP/1.0", "Upgrade", "websocket", false},
		{"GET", "HTTP/2.0", "Upgrade", "websocket", false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, "/", nil)
		r.Proto = tt.proto
		r.ProtoMajor, r.ProtoMinor, _ = http.ParseHTTPVersion(tt.proto)
		r.Header.Set("Connection", tt.conn)
		r.Header.Set("Upgrade", tt.upgrade)
		if got := isWebSocket(r); got != tt.want {
			t.Errorf("%s %s Connection %q Upgrade %q: got %v, want %v", tt.method, tt.proto, tt.conn, tt.upgrade, got, tt.want)
		}
	}
}

func TestIdleTimer(t *testing.T) {
	fired := make(chan time.Time, 1)
	start := time.Now()
	it := newIdleTimer(50*time.Millisecond, func() { fired <- time.Now() })
	for range 5 {
		time.Sleep(20 * time.Millisecond)
		it.touch()
	}
	last := time.Now()
	select {
	case at := <-fired:
		if at.Sub(last) < 45*time.Millisecond {
			t.Errorf("fired %v after the last touch, want at least 50ms (%v after start)", at.Sub(last), at.Sub(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never fired")
	}

	it = newIdleTimer(20*time.Millisecond, func() { t.Error("fired after stop") })
	it.stop()
	time.Sleep(50 * time.Millisecond)
}
