package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// h2Proxy serves a route to backend over HTTP/2 with TLS, and returns a
// client for it, which speaks HTTP/2.
func h2Proxy(t *testing.T, backend http.HandlerFunc) (*wsFront, string, *http.Client) {
	t.Helper()
	bsrv := httptest.NewServer(backend)
	t.Cleanup(bsrv.Close)
	f, srv := newTrackingFront(newHandler(t, testUpstream(bsrv.URL), &Route{}, slog.New(slog.DiscardHandler)))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return f, srv.URL, srv.Client()
}

func h2Do(t *testing.T, c *http.Client, req *http.Request) *http.Response {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.ProtoMajor != 2 {
		t.Fatalf("client spoke %s, want HTTP/2", resp.Proto)
	}
	return resp
}

// TestProxyHTTP2 checks what HTTP/2 clients rely on, against backends that
// the proxy reaches over HTTP/1.1.
func TestProxyHTTP2(t *testing.T) {
	t.Run("streaming", func(t *testing.T) {
		release := make(chan struct{})
		_, url, c := h2Proxy(t, func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor != 1 {
				t.Errorf("backend got %s, want HTTP/1.1", r.Proto)
			}
			io.WriteString(w, "first")
			w.(http.Flusher).Flush()
			<-release
			io.WriteString(w, "second")
		})
		req, _ := http.NewRequest("GET", url+"/", nil)
		resp := h2Do(t, c, req)
		first := make([]byte, 5)
		done := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(resp.Body, first)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil || string(first) != "first" {
				t.Fatalf("first chunk %q, %v", first, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("flushed chunk did not reach the HTTP/2 client")
		}
		close(release)
		if rest, _ := io.ReadAll(resp.Body); string(rest) != "second" {
			t.Errorf("rest %q, want second", rest)
		}
	})

	t.Run("trailers", func(t *testing.T) {
		_, url, c := h2Proxy(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Te") != "trailers" {
				t.Errorf("backend got TE %q, want trailers", r.Header.Get("Te"))
			}
			w.Header().Set("Trailer", "Grpc-Status")
			w.Header().Set("Content-Type", "application/grpc")
			io.WriteString(w, "message")
			w.Header().Set("Grpc-Status", "0")
		})
		req, _ := http.NewRequest("POST", url+"/svc/Method", strings.NewReader("call"))
		req.Header.Set("Te", "trailers")
		resp := h2Do(t, c, req)
		if body, _ := io.ReadAll(resp.Body); string(body) != "message" {
			t.Errorf("body %q", body)
		}
		if got := resp.Trailer.Get("Grpc-Status"); got != "0" {
			t.Errorf("trailer Grpc-Status = %q, want 0", got)
		}
	})

	t.Run("streamed upload", func(t *testing.T) {
		_, url, c := h2Proxy(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			io.WriteString(w, strings.ToUpper(string(b)))
		})
		pr, pw := io.Pipe()
		go func() {
			for _, s := range []string{"a", "b", "c"} {
				io.WriteString(pw, s)
				time.Sleep(5 * time.Millisecond)
			}
			pw.Close()
		}()
		req, _ := http.NewRequest("POST", url+"/", pr) // unknown length
		resp := h2Do(t, c, req)
		if body, _ := io.ReadAll(resp.Body); string(body) != "ABC" {
			t.Errorf("body %q, want ABC", body)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		canceled := make(chan struct{})
		front, url, c := h2Proxy(t, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			close(canceled)
		})
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", url+"/", nil)
		time.AfterFunc(50*time.Millisecond, cancel)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
			t.Fatal("request was not canceled")
		}
		select {
		case <-canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("canceling the HTTP/2 stream did not cancel the backend request")
		}
		if an := front.annotations(t); an.Error != "client_closed" {
			t.Errorf("error = %q, want client_closed", an.Error)
		}
	})
}
