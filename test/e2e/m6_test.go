package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These are the M6 exit tests: canary and shadow, auth and IP filtering,
// WebSockets, HTTP/2 and trace propagation, each through the whole stack.

// The example's credentials: alice with password s3cret, and key-one.
const (
	aliceHash  = "$2a$10$7PTiybTg/fasyyDtp5gxKO.dwkbXmCniNwUPJz/LHRnxoqhi4pe/6"
	keyDigest  = "sha256:9b346041bc9a49574eb2665b2ad2a0a3f9f9cce4e42f5d1f26deb8a256b5966a"
	traceparnt = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
)

// named answers every request with name.
func named(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name) }
}

// TestCanarySplit sends 2,000 clients through a 20% canary: about a fifth
// land on it, each client stays on its side, and the header and cookie
// overrides pick a side for one request.
func TestCanarySplit(t *testing.T) {
	stable, canary := startBackend(t, named("stable")), startBackend(t, named("canary"))
	front := startProxyYAML(t, fmt.Sprintf(`
trusted_proxies: [127.0.0.1]
upstreams:
  stable: { backends: [{ url: %q }] }
  canary: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: stable, canary: { upstream: canary, percent: 20, header: X-Canary, cookie: canary } }
`, stable.URL, canary.URL))
	client := newClient(t)
	side := func(ip string, hdr ...string) string {
		req, _ := http.NewRequest("GET", front+"/", nil)
		req.Header.Set("X-Forwarded-For", ip)
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	const clients = 2000
	first := make([]string, clients)
	n := 0
	for i := range clients {
		first[i] = side(fmt.Sprintf("10.%d.%d.1", i/256, i%256))
		if first[i] == "canary" {
			n++
		}
	}
	if share := float64(n) / clients; share < 0.17 || share > 0.23 {
		t.Errorf("canary share %.3f, want 0.20 ± 0.03", share)
	}
	for i := 0; i < clients; i += 50 {
		if got := side(fmt.Sprintf("10.%d.%d.1", i/256, i%256)); got != first[i] {
			t.Errorf("client %d moved from %s to %s", i, first[i], got)
		}
	}

	// A client on the stable side, for the overrides.
	var ip string
	for i, s := range first {
		if s == "stable" {
			ip = fmt.Sprintf("10.%d.%d.1", i/256, i%256)
			break
		}
	}
	for _, tt := range []struct {
		hdr  []string
		want string
	}{
		{[]string{"X-Canary", "always"}, "canary"},
		{[]string{"Cookie", "canary=always"}, "canary"},
		{[]string{"X-Canary", "never", "Cookie", "canary=always"}, "stable"}, // the header wins
		{[]string{"X-Canary", "maybe"}, "stable"},
	} {
		if got := side(ip, tt.hdr...); got != tt.want {
			t.Errorf("%v: got %s, want %s", tt.hdr, got, tt.want)
		}
	}
}

// TestShadowAddsNoLatency mirrors every request to a backend that takes a
// second to answer. The real requests stay fast, and copies beyond the
// shadow's concurrency are dropped instead of piling up.
func TestShadowAddsNoLatency(t *testing.T) {
	var copies atomic.Int64
	slow := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Prism-Shadow") == "1" {
			copies.Add(1)
		}
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	})
	fast := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	})
	front := startProxyYAML(t, fmt.Sprintf(`
upstreams:
  app: { backends: [{ url: %q }] }
  dark: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app, shadow: { upstream: dark, max_concurrency: 2, timeout: 2s } }
`, fast.URL, slow.URL))
	client := newClient(t)
	start := time.Now()
	for i := range 20 {
		resp, body := send(t, client, "POST", front+"/orders", "", strings.NewReader(fmt.Sprintf(`{"n":%d}`, i)))
		if resp.StatusCode != http.StatusOK || body != "ok" {
			t.Fatalf("request %d: %d %q", i, resp.StatusCode, body)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("20 requests took %v with a 1 s shadow backend, want well under a second", d)
	}
	eventually(t, "the shadow copies", func() bool { return copies.Load() == 2 })
}

// TestAuthMatrix runs the combinations of credentials against a route that
// takes a password or a key, behind a global deny list.
func TestAuthMatrix(t *testing.T) {
	type seen struct{ auth, key, user string }
	got := make(chan seen, 100)
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Auth-User")}
	})
	front := startProxyYAML(t, fmt.Sprintf(`
trusted_proxies: [127.0.0.1]
access: { deny: [192.0.2.0/24] }
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - match: { path_prefix: / }
    upstream: app
    rate_limit: { per_ip: 1000/s }
    auth:
      basic: { users: { alice: %q } }
      api_keys: { keys: { ci: %q } }
      identity_header: X-Auth-User
`, backend.URL, aliceHash, keyDigest))
	client := newClient(t)
	basic := func(user, pass string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}
	tests := []struct {
		name string
		ip   string
		hdr  []string
		want int
		user string
	}{
		{"nothing", "10.0.0.1", nil, 401, ""},
		{"password", "10.0.0.1", []string{"Authorization", basic("alice", "s3cret")}, 200, "alice"},
		{"wrong password", "10.0.0.1", []string{"Authorization", basic("alice", "nope")}, 401, ""},
		{"unknown user", "10.0.0.1", []string{"Authorization", basic("bob", "s3cret")}, 401, ""},
		{"key", "10.0.0.1", []string{"X-API-Key", "key-one"}, 200, "ci"},
		{"wrong key", "10.0.0.1", []string{"X-API-Key", "key-two"}, 401, ""},
		{"wrong key, good password", "10.0.0.1", []string{"X-API-Key", "key-two", "Authorization", basic("alice", "s3cret")}, 401, ""},
		{"spoofed identity", "10.0.0.1", []string{"X-API-Key", "key-one", "X-Auth-User", "root"}, 200, "ci"},
		{"denied address", "192.0.2.7", []string{"X-API-Key", "key-one"}, 403, ""},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest("GET", front+"/", nil)
		req.Header.Set("X-Forwarded-For", tt.ip)
		for i := 0; i < len(tt.hdr); i += 2 {
			req.Header.Set(tt.hdr[i], tt.hdr[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, resp.StatusCode, tt.want)
			continue
		}
		if tt.want == 401 && !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic realm=") {
			t.Errorf("%s: WWW-Authenticate %q", tt.name, resp.Header.Get("WWW-Authenticate"))
		}
		if tt.want != 200 {
			continue
		}
		s := <-got
		if s.user != tt.user || s.auth != "" || s.key != "" {
			t.Errorf("%s: backend saw user %q, Authorization %q, key %q; want %q and no credentials", tt.name, s.user, s.auth, s.key, tt.user)
		}
	}
	if len(got) != 0 {
		t.Errorf("%d refused requests reached the backend", len(got))
	}
}

// wsDial opens a WebSocket to url through a raw connection and returns it
// after the 101.
func wsDial(t *testing.T, url string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "GET /ws HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-Websocket-Accept") != "accepted" {
		t.Fatalf("handshake: %d, accept %q", resp.StatusCode, resp.Header.Get("Sec-Websocket-Accept"))
	}
	return c, br
}

func wsEcho(t *testing.T, c net.Conn, br *bufio.Reader, msg string) {
	t.Helper()
	io.WriteString(c, msg)
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo of %q: %q, %v", msg, buf, err)
	}
}

// TestWebSocket echoes through a WebSocket route, checks the idle cutoff,
// and that a shutdown waits for an open WebSocket until its deadline and
// then closes it.
func TestWebSocket(t *testing.T) {
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: accepted\r\n\r\n")
		brw.Flush()
		io.Copy(c, brw)
	})
	logs := newLogBuffer(t)
	inst := launch(t, fmt.Sprintf(`
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app, websocket: { idle_timeout: 200ms } }
`, backend.URL), logs)
	front := "http://" + inst.Addrs()[0].String()

	c, br := wsDial(t, front)
	for range 5 {
		wsEcho(t, c, br, "ping")
		time.Sleep(100 * time.Millisecond)
	}
	start := time.Now()
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("read data from an idle WebSocket")
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Errorf("idle WebSocket closed %v after the last echo, want about 100ms more", d)
	}
	// Logged once the handler returns, just after the close.
	eventually(t, "the WebSocket's access log line", func() bool {
		return strings.Contains(logs.String(), "websocket=true bytes_up=20 bytes_down=20")
	})

	c, br = wsDial(t, front)
	wsEcho(t, c, br, "hold")
	done := make(chan error, 1)
	shutdownStart := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		done <- inst.Shutdown(ctx)
	}()
	// Kept busy, so only the shutdown can end it.
	stop := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(stop) {
		wsEcho(t, c, br, "busy")
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed 1 hijacked connections") {
			t.Errorf("Shutdown = %v, want the WebSocket closed at the deadline", err)
		}
		if d := time.Since(shutdownStart); d < 150*time.Millisecond {
			t.Errorf("Shutdown returned after %v, before its deadline", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Error("WebSocket still open after Shutdown")
	}
}

// writeTestCert writes a self-signed certificate for localhost into dir and
// returns the cert and key paths, and a pool that trusts it.
func writeTestCert(t *testing.T, dir string) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	return certFile, keyFile, roots
}

// TestHTTP2 sends HTTP/2 to a TLS listener and to an h2c one, in front of
// an HTTP/1.1 backend: streaming, trailers and the trace context survive the
// change of protocol.
func TestHTTP2(t *testing.T) {
	type seen struct{ proto, traceparent string }
	got := make(chan seen, 10)
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Proto, r.Header.Get("Traceparent")}
		w.Header().Set("Trailer", "Grpc-Status")
		io.WriteString(w, "first,")
		w.(http.Flusher).Flush()
		io.WriteString(w, "second")
		w.Header().Set("Grpc-Status", "0")
	})
	certFile, keyFile, roots := writeTestCert(t, t.TempDir())
	inst := launch(t, fmt.Sprintf(`listeners:
  - { addr: "127.0.0.1:0", tls: { certs: [{ cert: %q, key: %q }] } }
  - { addr: "127.0.0.1:0", h2c: true }
upstreams:
  app: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: app }
`, certFile, keyFile, backend.URL), t.Output())
	addrs := inst.Addrs()

	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	tlsTr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}, ForceAttemptHTTP2: true}
	h2cTr := &http.Transport{Protocols: h2c}
	t.Cleanup(tlsTr.CloseIdleConnections)
	t.Cleanup(h2cTr.CloseIdleConnections)
	for _, tt := range []struct {
		name string
		url  string
		tr   *http.Transport
	}{
		{"tls", "https://" + addrs[0].String() + "/", tlsTr},
		{"h2c", "http://" + addrs[1].String() + "/", h2cTr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", tt.url, nil)
			req.Header.Set("Traceparent", traceparnt)
			resp, err := (&http.Client{Transport: tt.tr}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.ProtoMajor != 2 || string(body) != "first,second" || resp.Trailer.Get("Grpc-Status") != "0" {
				t.Errorf("got %s %q, trailer %q", resp.Proto, body, resp.Trailer.Get("Grpc-Status"))
			}
			s := <-got
			if s.proto != "HTTP/1.1" {
				t.Errorf("backend got %s, want HTTP/1.1", s.proto)
			}
			// Same trace, with Prism's span as the parent.
			if !strings.HasPrefix(s.traceparent, traceparnt[:36]) || s.traceparent == traceparnt || !strings.HasSuffix(s.traceparent, "-01") {
				t.Errorf("backend got traceparent %q, want trace %s with a new parent", s.traceparent, traceparnt[3:35])
			}
		})
	}
}
