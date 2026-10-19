package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/runtime"
)

// logBuffer collects log output written from server goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitFor polls logs until they contain want.
func (l *logBuffer) waitFor(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		out := l.String()
		if strings.Contains(out, want) || time.Now().After(deadline) {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newTestLogger() (*logBuffer, *slog.Logger) {
	buf := &logBuffer{}
	return buf, slog.New(slog.NewTextHandler(buf, nil))
}

func plainListener() config.Listener {
	return config.Listener{
		Addr:     "127.0.0.1:0",
		Timeouts: config.ListenerTimeouts{ReadHeader: 2 * time.Second, Idle: 3 * time.Second},
	}
}

// start runs a server for listeners and returns the base URL of each one.
func start(t *testing.T, listeners []config.Listener, h http.Handler, log *slog.Logger) (*Server, []string) {
	t.Helper()
	s, err := New(listeners, h, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	var urls []string
	for i, a := range s.Addrs() {
		scheme := "http"
		if listeners[i].TLS != nil {
			scheme = "https"
		}
		urls = append(urls, scheme+"://"+a.String())
	}
	return s, urls
}

func TestServerTimeoutsFromConfig(t *testing.T) {
	s, err := New([]config.Listener{plainListener()}, http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := s.servers[0]
	if srv.ReadHeaderTimeout != 2*time.Second || srv.IdleTimeout != 3*time.Second {
		t.Errorf("timeouts = %v, %v", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Error("whole-request timeouts must stay off for uploads and streams")
	}
}

func TestServerReadHeaderTimeout(t *testing.T) {
	l := plainListener()
	l.Timeouts.ReadHeader = 100 * time.Millisecond
	_, urls := start(t, []config.Listener{l}, http.NotFoundHandler(), slog.New(slog.DiscardHandler))

	conn, err := net.Dial("tcp", strings.TrimPrefix(urls[0], "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n") // never finished
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	io.ReadAll(conn)
	if d := time.Since(start); d > time.Second {
		t.Errorf("slow client held the connection for %v", d)
	}
}

func TestServerTLS(t *testing.T) {
	dir := t.TempDir()
	a, certA := writeCert(t, dir, "a", "a.test")
	b, certB := writeCert(t, dir, "b", "b.test")
	l := plainListener()
	l.TLS = &config.ListenerTLS{Certs: []config.CertPair{a, b}}
	_, urls := start(t, []config.Listener{l}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}), slog.New(slog.DiscardHandler))

	roots := x509.NewCertPool()
	roots.AddCert(certA)
	roots.AddCert(certB)
	client := func(cfg *tls.Config) *http.Client {
		cfg.RootCAs = roots
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}}
	}

	for _, name := range []string{"a.test", "b.test"} {
		resp, err := client(&tls.Config{ServerName: name}).Get(urls[0])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := resp.TLS.PeerCertificates[0].DNSNames[0]; got != name {
			t.Errorf("SNI %s: served the certificate for %s", name, got)
		}
		if string(body) != "HTTP/2.0" {
			t.Errorf("SNI %s: protocol %s, want HTTP/2.0", name, body)
		}
	}

	_, err := client(&tls.Config{ServerName: "a.test", MaxVersion: tls.VersionTLS11}).Get(urls[0])
	if err == nil {
		t.Error("a TLS 1.1 client was accepted")
	}
}

func TestStartPortInUse(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	l := plainListener()
	l.Addr = busy.Addr().String()
	s, err := New([]config.Listener{plainListener(), l}, http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Error("Start succeeded on a port in use")
	}
}

func TestShutdown(t *testing.T) {
	s, urls := start(t, []config.Listener{plainListener()}, http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(urls[0]); err == nil {
		t.Error("still serving after Shutdown")
	}
	select {
	case err := <-s.Err():
		t.Errorf("Shutdown reported as a listener failure: %v", err)
	default:
	}
}

func TestAccessLogProxied(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "hello")
	}))
	defer backend.Close()
	cfg, err := config.Parse(fmt.Appendf(nil, `
trusted_proxies: [127.0.0.1]
listeners: [{ addr: ":8080" }]
upstreams:
  api: { backends: [{ url: %q }] }
routes:
  - { match: { host: api.test, path_prefix: /v1/ }, upstream: api }
`, backend.URL), "")
	if err != nil {
		t.Fatal(err)
	}
	logs, log := newTestLogger()
	rt, err := runtime.Build(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	_, urls := start(t, []config.Listener{plainListener()}, runtime.NewCurrent(rt), log)

	req, _ := http.NewRequest("GET", urls[0]+"/v1/users?secret=1", nil)
	req.Host = "api.test"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	out := logs.waitFor(t, "msg=request")
	for _, want := range []string{
		"method=GET", "host=api.test", "path=/v1/users ", "proto=HTTP/1.1",
		"status=201", "bytes=5", "duration=",
		"client=203.0.113.9", // the peer is trusted, so its X-Forwarded-For is used
		"route=api.test/v1/", "upstream=api", "backend=" + backend.URL, "attempts=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("access log %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "secret") {
		t.Error("access log contains the query string")
	}
}

func TestAccessLogClientClosed(t *testing.T) {
	logs, log := newTestLogger()
	_, urls := start(t, []config.Listener{plainListener()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), log)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", urls[0], nil)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("request did not time out")
	}
	if out := logs.waitFor(t, "msg=request"); !strings.Contains(out, "status=499") {
		t.Errorf("access log %q, want status=499", out)
	}
}

func TestAccessLogAborted(t *testing.T) {
	logs, log := newTestLogger()
	_, urls := start(t, []config.Listener{plainListener()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "partial")
		panic(http.ErrAbortHandler)
	}), log)

	if resp, err := http.Get(urls[0]); err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Error("aborted response looked complete")
		}
	}
	if out := logs.waitFor(t, "msg=request"); !strings.Contains(out, "aborted=true") {
		t.Errorf("access log %q, want aborted=true", out)
	}
}

func TestAccessLogKeepsFlush(t *testing.T) {
	release := make(chan struct{})
	flushErr := make(chan error, 1)
	_, urls := start(t, []config.Listener{plainListener()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first\n")
		flushErr <- http.NewResponseController(w).Flush()
		<-release
	}), slog.New(slog.DiscardHandler))
	defer close(release)

	resp, err := http.Get(urls[0])
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := <-flushErr; err != nil {
		t.Fatalf("Flush through the access log: %v", err)
	}
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "first\n" {
			t.Errorf("got %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flushed data did not reach the client")
	}
}
