package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/balancer"
	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/router"
	"github.com/MuditGarg007/prism/internal/upstream"
)

func TestValidPath(t *testing.T) {
	valid := []string{
		"/", "/a", "/a/", "/a/b.c", "/a/..b", "/a/b..", "/.well-known/x", "/...", "/a/.b/",
	}
	invalid := []string{
		"", "*", "a", "a/b", "//", "/a//b", "/a/b//", "/.", "/./a", "/a/.", "/a/./",
		"/..", "/../a", "/a/..", "/a/../b", "/a/../",
	}
	for _, p := range valid {
		if !validPath(p) {
			t.Errorf("validPath(%q) = false, want true", p)
		}
	}
	for _, p := range invalid {
		if validPath(p) {
			t.Errorf("validPath(%q) = true, want false", p)
		}
	}
}

func TestValidPathAllocs(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() { validPath("/api/v1/users/42/") })
	if allocs != 0 {
		t.Errorf("validPath allocates %v times, want 0", allocs)
	}
}

// FuzzValidPath checks validPath against path.Clean: a valid path is one that
// Clean leaves alone, apart from a trailing "/".
func FuzzValidPath(f *testing.F) {
	for _, s := range []string{"/", "/a/b", "/a/../b", "//", "/a/", "/./", "/.."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		c := path.Clean(p)
		want := strings.HasPrefix(p, "/") && (c == p || (p != "//" && c+"/" == p))
		if got := validPath(p); got != want {
			t.Fatalf("validPath(%q) = %v, want %v (Clean gives %q)", p, got, want, c)
		}
	})
}

func TestTargetURL(t *testing.T) {
	backend, _ := url.Parse("http://10.0.0.1:8080")
	tests := []struct {
		name   string
		in     string
		prefix string
		strip  bool
		want   string
	}{
		{"no strip", "/api/users?id=1", "/api/", false, "http://10.0.0.1:8080/api/users?id=1"},
		{"strip", "/api/users?id=1", "/api/", true, "http://10.0.0.1:8080/users?id=1"},
		{"strip to root", "/api", "/api", true, "http://10.0.0.1:8080/"},
		{"strip without slash", "/apiv2/x", "/api", true, "http://10.0.0.1:8080/v2/x"},
		{"root prefix", "/a/b", "/", true, "http://10.0.0.1:8080/a/b"},
		{"escaped slash kept", "/api/a%2Fb", "/api/", false, "http://10.0.0.1:8080/api/a%2Fb"},
		{"escaped slash stripped", "/api/a%2Fb", "/api/", true, "http://10.0.0.1:8080/a%2Fb"},
		{"escaped prefix drops raw path", "/%61pi/a%2Fb", "/api/", true, "http://10.0.0.1:8080/a/b"},
		{"empty query kept", "/x?", "/", false, "http://10.0.0.1:8080/x?"},
		{"raw query untouched", "/x?a=%20b&c", "/", false, "http://10.0.0.1:8080/x?a=%20b&c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := url.ParseRequestURI(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := targetURL(backend, in, tt.prefix, tt.strip).String(); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func testUpstream(backendURL string) *config.Upstream {
	return &config.Upstream{
		Backends: []config.Backend{{URL: backendURL, Weight: 1}},
		Timeouts: config.Timeouts{
			Connect:        2 * time.Second,
			ResponseHeader: 5 * time.Second,
			IdleConn:       10 * time.Second,
		},
		MaxIdleConnsPerHost: 4,
	}
}

// newHandler builds a handler with one route, rt, to the upstream u.
func newHandler(t testing.TB, u *config.Upstream, rt *Route, log *slog.Logger, trusted ...netip.Prefix) *Handler {
	t.Helper()
	pool, err := upstream.NewPool("test", u, balancer.New(config.BalancerRoundRobin), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rt.Pool = pool
	if rt.PathPrefix == "" {
		rt.PathPrefix = "/"
	}
	table, err := router.Compile([]router.Entry[*Route]{{PathPrefix: rt.PathPrefix, Value: rt}})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(table, NewForwarding(trusted, false), log)
}

// serve starts a server for h and returns its URL.
func serve(t testing.TB, h http.Handler) string {
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front.URL
}

// proxyFor starts a proxy with one route to backend and returns its URL.
func proxyFor(t testing.TB, backend *httptest.Server, rt *Route, trusted ...netip.Prefix) string {
	t.Helper()
	return serve(t, newHandler(t, testUpstream(backend.URL), rt, slog.New(slog.DiscardHandler), trusted...))
}

func get(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestProxyRequestHeaders(t *testing.T) {
	seen := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
	}))
	defer backend.Close()
	front := proxyFor(t, backend, &Route{
		Request: NewHeaderOps(map[string]string{"X-Forwarded-Proto": "https", "X-Added": "1"}, []string{"X-Remove"}),
	})

	req, _ := http.NewRequest("GET", front+"/x", nil)
	req.Header.Set("Connection", "X-Secret")
	req.Header.Set("X-Secret", "s")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Remove", "r")
	req.Header.Set("X-Kept", "k")
	req.Header["User-Agent"] = []string{""}
	get(t, req)
	r := <-seen

	frontHost := strings.TrimPrefix(front, "http://")
	want := map[string]string{
		"X-Secret":          "",
		"X-Remove":          "",
		"X-Kept":            "k",
		"X-Added":           "1",
		"X-Forwarded-For":   "127.0.0.1", // the untrusted client's value is replaced
		"X-Forwarded-Proto": "https",     // the route rule runs after forwarding
		"X-Forwarded-Host":  frontHost,
	}
	for k, v := range want {
		if got := r.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if ua, ok := r.Header["User-Agent"]; ok {
		t.Errorf("User-Agent %q was added", ua)
	}
}

func TestProxyHost(t *testing.T) {
	seen := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
	}))
	defer backend.Close()
	backendHost := strings.TrimPrefix(backend.URL, "http://")

	tests := []struct {
		name string
		rt   *Route
		want string
	}{
		{"backend host by default", &Route{}, backendHost},
		{"preserve_host", &Route{PreserveHost: true}, "client.test"},
		{"Host rule", &Route{PreserveHost: true, Request: NewHeaderOps(map[string]string{"host": "rule.test"}, nil)}, "rule.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			front := proxyFor(t, backend, tt.rt)
			req, _ := http.NewRequest("GET", front+"/", nil)
			req.Host = "client.test"
			get(t, req)
			r := <-seen
			if r.Host != tt.want {
				t.Errorf("Host = %q, want %q", r.Host, tt.want)
			}
			if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "client.test" {
				t.Errorf("X-Forwarded-Host = %q, want client.test", xfh)
			}
			if _, ok := r.Header["Host"]; ok {
				t.Error("Host left in the header map")
			}
		})
	}
}

func TestProxyStripPrefix(t *testing.T) {
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.RequestURI
	}))
	defer backend.Close()
	front := proxyFor(t, backend, &Route{PathPrefix: "/api/", StripPrefix: true})

	req, _ := http.NewRequest("GET", front+"/api/a%2Fb?x=1&y", nil)
	get(t, req)
	if got := <-seen; got != "/a%2Fb?x=1&y" {
		t.Errorf("backend got %q, want /a%%2Fb?x=1&y", got)
	}
}

func TestProxyRejectsPaths(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer backend.Close()
	front := proxyFor(t, backend, &Route{PathPrefix: "/public/"})

	for _, p := range []string{"/public/../admin", "/public/%2e%2e/admin", "/public/..%2Fadmin", "/public//x", "/public/./x"} {
		req, _ := http.NewRequest("GET", front+p, nil)
		if resp, _ := get(t, req); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", p, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", front+"/other", nil)
	if resp, _ := get(t, req); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unrouted path: status %d, want 404", resp.StatusCode)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("backend was hit %d times", n)
	}
}

func TestProxyResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Connection", "X-Hop")
		h.Set("X-Hop", "1")
		h.Set("Keep-Alive", "timeout=5")
		h.Set("X-Drop", "1")
		h.Set("X-Kept", "1")
		h.Set("Trailer", "X-Checksum")
		// No Content-Type, so the proxy must not sniff one either.
		h["Content-Type"] = nil
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "<html>hi</html>")
		h.Set("X-Checksum", "abc")
	}))
	defer backend.Close()
	front := proxyFor(t, backend, &Route{
		Response: NewHeaderOps(map[string]string{"X-Added": "1"}, []string{"X-Drop"}),
	})

	req, _ := http.NewRequest("GET", front+"/", nil)
	resp, body := get(t, req)
	if resp.StatusCode != http.StatusTeapot || body != "<html>hi</html>" {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
	for k, want := range map[string]string{
		"X-Hop": "", "Keep-Alive": "", "X-Drop": "", "Content-Type": "",
		"X-Kept": "1", "X-Added": "1",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := resp.Trailer.Get("X-Checksum"); got != "abc" {
		t.Errorf("trailer X-Checksum = %q, want abc", got)
	}
}

func TestProxyBody(t *testing.T) {
	type got struct {
		length  int64
		chunked bool
		sum     string
	}
	seen := make(chan got, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		io.Copy(h, r.Body)
		seen <- got{r.ContentLength, len(r.TransferEncoding) > 0, hex.EncodeToString(h.Sum(nil))}
		w.Header().Set("Content-Length", "5")
		io.WriteString(w, "hello")
	}))
	defer backend.Close()
	front := proxyFor(t, backend, &Route{})

	data := bytes.Repeat([]byte("prism"), 200_000)
	sum := sha256.Sum256(data)
	tests := []struct {
		name string
		body io.Reader
		want got
	}{
		{"no body", nil, got{0, false, hex.EncodeToString(sha256.New().Sum(nil))}},
		{"content length", bytes.NewReader(data), got{int64(len(data)), false, hex.EncodeToString(sum[:])}},
		// MultiReader hides the length, so the client sends it chunked.
		{"chunked", io.MultiReader(bytes.NewReader(data)), got{-1, true, hex.EncodeToString(sum[:])}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", front+"/", tt.body)
			resp, body := get(t, req)
			if body != "hello" || resp.ContentLength != 5 {
				t.Errorf("response %q, length %d", body, resp.ContentLength)
			}
			if g := <-seen; g != tt.want {
				t.Errorf("backend saw %+v, want %+v", g, tt.want)
			}
		})
	}
}

func TestProxyStreamsEvents(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: 1\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: 2\n\n")
	}))
	defer backend.Close()
	defer close(release)
	front := proxyFor(t, backend, &Route{})

	resp, err := http.Get(front + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "data: 1\n" {
			t.Errorf("first line %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event not delivered while the backend was blocked")
	}
}

func TestProxyAbortsTruncatedBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "only ten..")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer backend.Close()
	front := proxyFor(t, backend, &Route{})

	// The abort may land before or after the buffered headers reach the
	// client. Either way the response must fail, not look complete.
	resp, err := http.Get(front + "/")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("status %d, read error %v, want unexpected EOF", resp.StatusCode, err)
	}
}

func TestProxyReleasesBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	rt := &Route{}
	front := proxyFor(t, backend, rt)

	for range 3 {
		req, _ := http.NewRequest("GET", front+"/", nil)
		get(t, req)
	}
	if n := rt.Pool.Backends[0].Inflight(); n != 0 {
		t.Errorf("inflight = %d after all requests finished", n)
	}
}

func BenchmarkValidPath(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		validPath("/api/v1/users/42/orders")
	}
}

func BenchmarkTargetURL(b *testing.B) {
	backend, _ := url.Parse("http://10.0.0.1:8080")
	in, _ := url.ParseRequestURI("/api/v1/users?id=42")
	b.ReportAllocs()
	for b.Loop() {
		targetURL(backend, in, "/api/", true)
	}
}

// BenchmarkProxy measures a whole request through the proxy over loopback,
// so most of the time and allocations belong to net/http.
func BenchmarkProxy(b *testing.B) {
	payload := strings.Repeat("x", 1024)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		io.WriteString(w, payload)
	}))
	defer backend.Close()
	front := proxyFor(b, backend, &Route{})
	b.ReportAllocs()
	for b.Loop() {
		resp, err := http.Get(front + "/")
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
