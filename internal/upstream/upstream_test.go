package upstream

import (
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/resilience"
)

// firstBalancer always picks the first usable backend.
type firstBalancer []*Backend

func newFirst(bs []*Backend) Balancer { return firstBalancer(bs) }

func (fb firstBalancer) Pick(c Choice) (*Backend, error) {
	for _, b := range fb {
		if c.Usable(b) {
			return b, nil
		}
	}
	return nil, ErrNoBackend
}

func testUpstream(backends ...config.Backend) *config.Upstream {
	return &config.Upstream{
		Backends: backends,
		Timeouts: config.Timeouts{
			Connect:        2 * time.Second,
			ResponseHeader: 3 * time.Second,
			IdleConn:       4 * time.Second,
		},
		MaxIdleConnsPerHost: 16,
	}
}

func TestNewPool(t *testing.T) {
	u := testUpstream(
		config.Backend{URL: "http://127.0.0.1:9000", Weight: 3},
		config.Backend{URL: "https://10.0.0.2:8443", Weight: 1},
	)
	p, err := NewPool("api", u, newFirst, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if len(p.Backends) != 2 {
		t.Fatalf("got %d backends, want 2", len(p.Backends))
	}
	plain, secure := p.Backends[0], p.Backends[1]
	if plain.URL.Host != "127.0.0.1:9000" || plain.Weight != 3 {
		t.Errorf("backend 0 = %s weight %d", plain, plain.Weight)
	}
	if !plain.Available() || !secure.Available() {
		t.Error("new backends must be available")
	}

	tr := plain.Transport
	if tr.Proxy != nil {
		t.Error("transport must not use a proxy from the environment")
	}
	if !tr.DisableCompression {
		t.Error("transport must not add Accept-Encoding")
	}
	if tr.ResponseHeaderTimeout != 3*time.Second || tr.IdleConnTimeout != 4*time.Second || tr.MaxIdleConnsPerHost != 16 {
		t.Errorf("transport timeouts or pool size not taken from config: %+v", tr)
	}
	if tr.Protocols == nil || !tr.Protocols.HTTP1() || tr.Protocols.HTTP2() {
		t.Errorf("transport protocols = %v, want HTTP/1 only", tr.Protocols)
	}
	if tr.TLSClientConfig != nil {
		t.Error("plain http backend got a TLS config")
	}
	if c := secure.Transport.TLSClientConfig; c == nil || c.MinVersion != tls.VersionTLS12 {
		t.Errorf("https backend TLS config = %+v, want MinVersion TLS 1.2", c)
	}

	if b, err := p.Pick(Choice{}); err != nil || b != plain {
		t.Errorf("Pick = %v, %v; want backend 0", b, err)
	}
	if b, err := p.Pick(Choice{Tried: []*Backend{plain}}); err != nil || b != secure {
		t.Errorf("Pick after trying backend 0 = %v, %v; want backend 1", b, err)
	}
}

func TestBackendInflight(t *testing.T) {
	var b Backend
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			b.Acquire()
			b.Release()
		})
	}
	b.Acquire()
	wg.Wait()
	if got := b.Inflight(); got != 1 {
		t.Errorf("inflight = %d, want 1", got)
	}
}

func TestBackendHealth(t *testing.T) {
	var b Backend
	if !b.Available() {
		t.Fatal("zero Backend must be available")
	}
	b.SetHealthy(false)
	if b.Available() {
		t.Error("unhealthy backend is available")
	}
	b.SetHealthy(true)
	if !b.Available() {
		t.Error("healthy backend is unavailable")
	}
}

// TestBackendTLS checks certificate verification against a custom CA and the
// name sent in SNI. httptest's certificate is valid for 127.0.0.1 and
// example.com.
// TestPoolResilience checks that a pool builds a breaker per backend and one
// outlier detector for the pool, that Report feeds both, and that either one
// takes a backend out of Available and logs why.
func TestPoolResilience(t *testing.T) {
	backends := []config.Backend{{URL: "http://127.0.0.1:9000", Weight: 1}, {URL: "http://127.0.0.1:9001", Weight: 1}}
	report := func(b *Backend, out resilience.Outcome, n int) {
		for range n {
			adm, ok := b.Admit()
			if !ok {
				t.Fatalf("%s refused a request", b)
			}
			b.Report(adm, out)
		}
	}
	newPool := func(u *config.Upstream) (*Pool, *strings.Builder) {
		var logs strings.Builder
		p, err := NewPool("api", u, newFirst, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p, &logs
	}

	t.Run("none configured", func(t *testing.T) {
		p, _ := newPool(testUpstream(backends...))
		b := p.Backends[0]
		report(b, resilience.Failure, 100)
		if b.Breaker() != nil || !b.Available() {
			t.Error("a pool without circuit_breaker or outlier took a backend out")
		}
	})

	t.Run("breaker", func(t *testing.T) {
		u := testUpstream(backends...)
		u.CircuitBreaker = &config.CircuitBreaker{FailureRatio: 0.5, MinRequests: 2, OpenFor: time.Hour, Window: 10 * time.Second, HalfOpenProbes: 1}
		p, logs := newPool(u)
		a, b := p.Backends[0], p.Backends[1]
		if a.Breaker() == b.Breaker() {
			t.Fatal("backends share a breaker")
		}
		report(a, resilience.Success, 1)
		report(a, resilience.Failure, 1)
		if a.Available() || !b.Available() {
			t.Errorf("available: %v, %v; want only the failing backend out", a.Available(), b.Available())
		}
		if _, ok := a.Admit(); ok {
			t.Error("open breaker admitted a request")
		}
		if got := logs.String(); !strings.Contains(got, `msg="circuit breaker state changed" upstream=api backend=http://127.0.0.1:9000 from=closed to=open`) {
			t.Errorf("log = %q, want the breaker opening", got)
		}
	})

	t.Run("outlier", func(t *testing.T) {
		u := testUpstream(backends...)
		u.Outlier = &config.Outlier{ConsecutiveFailures: 3, BaseEjection: time.Hour, MaxEjection: time.Hour, MaxEjectionPercent: 50}
		p, logs := newPool(u)
		a, b := p.Backends[0], p.Backends[1]
		report(a, resilience.Failure, 3)
		if a.Available() {
			t.Error("backend still available after 3 failures in a row")
		}
		// The cap is pool wide: with one of two out, the other stays.
		report(b, resilience.Failure, 3)
		if !b.Available() {
			t.Error("max_ejection_percent did not keep the last backend in")
		}
		if got := logs.String(); !strings.Contains(got, `msg="backend ejected after consecutive failures" upstream=api backend=http://127.0.0.1:9000 for=1h0m0s`) {
			t.Errorf("log = %q, want the ejection", got)
		}
	})
}

func TestChoiceUsable(t *testing.T) {
	up, down, tried := &Backend{}, &Backend{}, &Backend{}
	down.SetHealthy(false)
	c := Choice{Tried: []*Backend{tried}}
	for _, tt := range []struct {
		name string
		b    *Backend
		want bool
	}{
		{"available", up, true},
		{"unhealthy", down, false},
		{"already tried", tried, false},
	} {
		if got := c.Usable(tt.b); got != tt.want {
			t.Errorf("%s: Usable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBackendTLS(t *testing.T) {
	sni := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni <- hello.ServerName
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		tls     *config.BackendTLS
		wantSNI string // sent for an IP URL only when server_name is set
		wantErr string
	}{
		{"system roots", nil, "", "certificate"},
		{"custom ca", &config.BackendTLS{CAFile: caFile}, "", ""},
		{"server name", &config.BackendTLS{CAFile: caFile, ServerName: "example.com"}, "example.com", ""},
		{"wrong server name", &config.BackendTLS{CAFile: caFile, ServerName: "wrong.test"}, "wrong.test", "wrong.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewPool("tls", testUpstream(config.Backend{URL: srv.URL, Weight: 1, TLS: tt.tls}), newFirst, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			req, _ := http.NewRequest("GET", srv.URL, nil)
			resp, err := p.Backends[0].Transport.RoundTrip(req)
			if got := <-sni; got != tt.wantSNI {
				t.Errorf("SNI = %q, want %q", got, tt.wantSNI)
			}
			if tt.wantErr != "" {
				if err == nil {
					resp.Body.Close()
					t.Fatalf("got no error, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
				t.Errorf("body = %q, want ok", body)
			}
		})
	}
}

func TestBadCAFile(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		file string
		want error
	}{
		{"no certificates", garbage, errNoCerts},
		{"missing file", filepath.Join(dir, "missing.pem"), fs.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := testUpstream(
				config.Backend{URL: "http://127.0.0.1:9000", Weight: 1},
				config.Backend{URL: "https://127.0.0.1:9443", Weight: 1, TLS: &config.BackendTLS{CAFile: tt.file}},
			)
			_, err := NewPool("api", u, newFirst, slog.New(slog.DiscardHandler))
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), "upstream api: backend 1") {
				t.Errorf("error %q does not name the backend", err)
			}
		})
	}
}
