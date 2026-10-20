// Package e2e runs the whole proxy, from YAML config to listening server,
// against real backends over loopback. These tests are the M1 and M2 (in
// m2_test.go) exit criteria, and each one asserts on what the backend saw.
package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/admin"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/runtime"
	"github.com/MuditGarg007/prism/internal/server"
)

// startProxy runs prism with a single catch-all route to backend and returns
// its base URL. extra is prepended to the config as top-level YAML. The proxy
// is shut down when the test ends, before backends registered earlier.
func startProxy(t *testing.T, backend, extra string) string {
	t.Helper()
	return startProxyYAML(t, fmt.Sprintf(`%s
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app }
`, extra, backend))
}

// startProxyYAML runs prism from body, a config with everything but the
// listeners, and returns its base URL. It is shut down like startProxy's.
func startProxyYAML(t *testing.T, body string) string {
	t.Helper()
	return startProxyLogged(t, body, t.Output())
}

// startProxyLogged is startProxyYAML with prism's log written to logs.
func startProxyLogged(t *testing.T, body string, logs io.Writer) string {
	t.Helper()
	return "http://" + launch(t, body, logs).Addrs()[0].String()
}

// launch runs prism from body, as main does, with its admin API when body
// configures one. It is shut down like startProxy's.
func launch(t *testing.T, body string, logs io.Writer) *instance {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.yaml")
	writeConfig(t, path, body)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(logs, nil))
	metrics := observability.NewMetrics()
	rt, err := runtime.Build(cfg, log, runtime.Options{Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	cur := runtime.NewCurrent(rt)
	srv, err := server.New(cfg.Listeners, cfg.ReadyPath, cur, log)
	if err != nil {
		rt.Close()
		t.Fatal(err)
	}
	reloader := runtime.NewReloader(path, cur, metrics, srv.ReloadCerts, log)
	if a := cfg.Admin; a != nil {
		srv.Admin(a.Listen, admin.New(cur, metrics, reloader.Reload, log))
	}
	if err := srv.Start(); err != nil {
		rt.Close()
		t.Fatal(err)
	}
	rt.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		cur.Load().Close()
	})
	return &instance{Server: srv, path: path}
}

// instance is a running prism and its config file.
type instance struct {
	*server.Server
	path string
}

// writeConfig writes body, a config with everything but the listeners, to
// path, with a listener on a free loopback port.
func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	yaml := "listeners: [{ addr: \"127.0.0.1:0\" }]\n" + body
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startBackend runs h on a loopback server that is closed when the test
// ends. Register it before startProxy so the proxy shuts down first.
func startBackend(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	b := httptest.NewServer(h)
	t.Cleanup(b.Close)
	return b
}

// newClient returns a client with its own connection pool.
func newClient(t *testing.T) *http.Client {
	tr := &http.Transport{DisableCompression: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// genReader produces n bytes without holding them, so a huge body costs no
// memory. The bytes are whatever is already in the caller's buffer; only the
// count matters.
type genReader struct{ n int64 }

func (g *genReader) Read(p []byte) (int, error) {
	if g.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.n {
		p = p[:g.n]
	}
	g.n -= int64(len(p))
	return len(p), nil
}

// heapWatch samples the live heap in the background and reports the peak
// growth over the heap measured at start.
type heapWatch struct {
	base uint64
	peak atomic.Uint64
	stop chan struct{}
	done chan struct{}
}

func watchHeap() *heapWatch {
	goruntime.GC()
	var ms goruntime.MemStats
	goruntime.ReadMemStats(&ms)
	w := &heapWatch{base: ms.HeapAlloc, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			var ms goruntime.MemStats
			goruntime.ReadMemStats(&ms)
			if ms.HeapAlloc > w.peak.Load() {
				w.peak.Store(ms.HeapAlloc)
			}
			select {
			case <-w.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

// growth stops sampling and returns the peak heap growth in bytes.
func (w *heapWatch) growth() uint64 {
	close(w.stop)
	<-w.done
	if p := w.peak.Load(); p > w.base {
		return p - w.base
	}
	return 0
}

const (
	bigBody       = 1 << 30
	maxHeapGrowth = 32 << 20
)

func TestStreamLargeBodies(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 2 GB through the proxy")
	}

	t.Run("upload", func(t *testing.T) {
		var got atomic.Int64
		backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				t.Errorf("backend read body: %v", err)
			}
			got.Store(n)
		})
		base := startProxy(t, backend.URL, "")
		client := newClient(t)

		watch := watchHeap()
		req, err := http.NewRequest(http.MethodPost, base+"/upload", &genReader{n: bigBody})
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = bigBody
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		growth := watch.growth()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
		if n := got.Load(); n != bigBody {
			t.Errorf("backend received %d bytes, want %d", n, bigBody)
		}
		t.Logf("heap growth %.1f MB", float64(growth)/(1<<20))
		if growth > maxHeapGrowth {
			t.Errorf("heap grew %d bytes, want at most %d", growth, maxHeapGrowth)
		}
	})

	// The download has no Content-Length, so it also covers the proxy's
	// flush-per-write path for chunked responses.
	t.Run("download", func(t *testing.T) {
		var sent atomic.Int64
		backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			n, err := io.Copy(w, &genReader{n: bigBody})
			if err != nil {
				t.Errorf("backend write body: %v", err)
			}
			sent.Store(n)
		})
		base := startProxy(t, backend.URL, "")
		client := newClient(t)

		watch := watchHeap()
		resp, err := client.Get(base + "/download")
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		growth := watch.growth()

		if err != nil {
			t.Fatalf("client read body: %v", err)
		}
		if resp.ContentLength != -1 {
			t.Errorf("Content-Length %d, want chunked", resp.ContentLength)
		}
		if n != bigBody {
			t.Errorf("client received %d bytes, want %d", n, bigBody)
		}
		if s := sent.Load(); s != bigBody {
			t.Errorf("backend sent %d bytes, want %d", s, bigBody)
		}
		t.Logf("heap growth %.1f MB", float64(growth)/(1<<20))
		if growth > maxHeapGrowth {
			t.Errorf("heap grew %d bytes, want at most %d", growth, maxHeapGrowth)
		}
	})
}

func TestSSEPassthrough(t *testing.T) {
	release := make(chan struct{})
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: 1\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: 2\n\n")
	})
	base := startProxy(t, backend.URL, "")
	// Registered after the proxy, so it runs first: a failed test must not
	// leave the proxy waiting on a blocked backend during shutdown.
	t.Cleanup(unblock)
	client := newClient(t)

	// A proxy that buffers never sends the response headers, so without a
	// deadline a failure would hang instead of failing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines <- sc.Text()
			}
		}
	}()
	next := func() string {
		t.Helper()
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended early")
			}
			return l
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for an event")
			return ""
		}
	}

	// The backend is still blocked, so event 1 can only arrive here if the
	// proxy flushed it instead of buffering.
	if got := next(); got != "data: 1" {
		t.Fatalf("first event %q, want %q", got, "data: 1")
	}
	unblock()
	if got := next(); got != "data: 2" {
		t.Fatalf("second event %q, want %q", got, "data: 2")
	}
}

func TestCancelReachesBackend(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	})
	base := startProxy(t, backend.URL, "")
	client := newClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the backend")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("backend context was not canceled within 1s")
	}
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("client error %v, want context.Canceled", err)
	}
}

func TestSpoofedForwardingHeaders(t *testing.T) {
	tests := []struct {
		name      string
		extra     string
		xff       string
		wantXFF   string
		wantFwded string
	}{
		{
			name:    "untrusted peer replaces the chain",
			xff:     "1.2.3.4",
			wantXFF: "127.0.0.1",
		},
		{
			name:    "untrusted peer with a forged multi-hop chain",
			xff:     "1.2.3.4, 10.0.0.1",
			wantXFF: "127.0.0.1",
		},
		{
			name:      "trusted peer appends to the chain",
			extra:     `trusted_proxies: ["127.0.0.1"]`,
			xff:       "1.2.3.4",
			wantXFF:   "1.2.3.4, 127.0.0.1",
			wantFwded: "for=1.2.3.4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := make(chan http.Header, 1)
			backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
			})
			base := startProxy(t, backend.URL, tt.extra)
			client := newClient(t)

			req, err := http.NewRequest(http.MethodGet, base+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Forwarded-For", tt.xff)
			req.Header.Set("Forwarded", "for=1.2.3.4")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			h := <-seen
			if got := strings.Join(h.Values("X-Forwarded-For"), ", "); got != tt.wantXFF {
				t.Errorf("backend X-Forwarded-For %q, want %q", got, tt.wantXFF)
			}
			if got := h.Get("Forwarded"); got != tt.wantFwded {
				t.Errorf("backend Forwarded %q, want %q", got, tt.wantFwded)
			}
		})
	}
}

func TestKeepAliveReuse(t *testing.T) {
	var conns atomic.Int64
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	backend.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	backend.Start()
	t.Cleanup(backend.Close)
	base := startProxy(t, backend.URL, "")
	client := newClient(t)

	for i := range 100 {
		resp, err := client.Get(base + "/")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != "ok" {
			t.Fatalf("request %d: body %q, err %v", i, body, err)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("backend accepted %d connections, want 1", n)
	}
}
