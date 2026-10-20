package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/server"
)

// startServer serves h on a loopback port with /ready as the ready path and
// returns the server and its base URL.
func startServer(t *testing.T, h http.HandlerFunc) (*server.Server, string) {
	t.Helper()
	l := config.Listener{Addr: "127.0.0.1:0", Timeouts: config.ListenerTimeouts{ReadHeader: time.Second, Idle: time.Second}}
	srv, err := server.New([]config.Listener{l}, "/ready", h, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv, "http://" + srv.Addrs()[0].String()
}

func status(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// shutdownAsync runs shutdown in the background and returns its result.
func shutdownAsync(srv *server.Server, sd config.Shutdown, sigs chan os.Signal) <-chan error {
	done := make(chan error, 1)
	go func() { done <- shutdown(srv, sd, sigs, slog.New(slog.DiscardHandler)) }()
	return done
}

// TestShutdownDrainDelay checks that during the drain delay requests are
// still served while the ready path says 503.
func TestShutdownDrainDelay(t *testing.T) {
	srv, base := startServer(t, func(w http.ResponseWriter, r *http.Request) {})
	if got := status(t, base+"/ready"); got != 200 {
		t.Fatalf("ready path before shutdown: %d", got)
	}
	start := time.Now()
	done := shutdownAsync(srv, config.Shutdown{DrainDelay: 200 * time.Millisecond, Timeout: time.Second}, make(chan os.Signal, 1))
	time.Sleep(20 * time.Millisecond)
	if got := status(t, base+"/ready"); got != http.StatusServiceUnavailable {
		t.Errorf("ready path while draining: %d, want 503", got)
	}
	if got := status(t, base+"/"); got != 200 {
		t.Errorf("request while draining: %d, want it served", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("stopped after %v, before the drain delay", d)
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Error("still accepting connections after shutdown")
	}
}

// TestShutdownWaitsForRequests checks that a request in flight when
// shutdown starts gets to finish.
func TestShutdownWaitsForRequests(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv, base := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	})
	got := make(chan string, 1)
	go func() {
		resp, err := http.Get(base + "/")
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got <- string(b)
	}()
	<-started
	done := shutdownAsync(srv, config.Shutdown{Timeout: 5 * time.Second}, make(chan os.Signal, 1))
	time.Sleep(30 * time.Millisecond)
	close(release)
	if body := <-got; body != "done" {
		t.Errorf("in-flight request got %q", body)
	}
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// TestShutdownTimeout checks that a request that outlives the timeout is
// cut off and reported.
func TestShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	srv, base := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	go http.Get(base + "/")
	<-started
	start := time.Now()
	err := <-shutdownAsync(srv, config.Shutdown{Timeout: 50 * time.Millisecond}, make(chan os.Signal, 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("shutdown = %v, want the deadline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v to give up", d)
	}
}

// TestShutdownSecondSignal checks that a second signal ends a long drain
// delay and a stuck request at once.
func TestShutdownSecondSignal(t *testing.T) {
	for name, sd := range map[string]config.Shutdown{
		"during the drain delay": {DrainDelay: time.Hour, Timeout: time.Hour},
		"during the timeout":     {Timeout: time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			srv, base := startServer(t, func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			})
			go http.Get(base + "/")
			<-started
			sigs := make(chan os.Signal, 1)
			done := shutdownAsync(srv, sd, sigs)
			time.Sleep(20 * time.Millisecond)
			sigs <- syscall.SIGTERM
			select {
			case err := <-done:
				if !errors.Is(err, errInterrupted) {
					t.Errorf("shutdown = %v, want errInterrupted", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("second signal did not stop the shutdown")
			}
		})
	}
}
