// Package server runs the HTTP and HTTPS listeners that feed requests to the
// proxy.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
)

// Server is one http.Server per configured listener, all serving the same
// handler.
type Server struct {
	log      *slog.Logger
	servers  []*http.Server
	certs    []*atomic.Pointer[certStore] // by listener, nil without TLS
	addrs    []net.Addr
	errc     chan error
	draining atomic.Bool
	hijacked connSet
	admin    bool // the last server is the admin API
}

// New prepares a server for each listener and loads their certificates.
// Nothing listens until Start. When readyPath is set, every listener answers
// it before routing: 200 until Drain, 503 after.
func New(listeners []config.Listener, readyPath string, h http.Handler, log *slog.Logger) (*Server, error) {
	s := &Server{log: log, errc: make(chan error, len(listeners))}
	handler := accessLog(h, log)
	if readyPath != "" {
		handler = s.readiness(readyPath, handler)
	}
	for i, l := range listeners {
		srv := &http.Server{
			Addr:              l.Addr,
			Handler:           handler,
			ReadHeaderTimeout: l.Timeouts.ReadHeader,
			IdleTimeout:       l.Timeouts.Idle,
			// Mostly TLS handshake failures from clients and scanners.
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
		var certs *atomic.Pointer[certStore]
		if l.TLS != nil {
			store, err := loadCerts(l.TLS.Certs)
			if err != nil {
				return nil, fmt.Errorf("listeners[%d]: %w", i, err)
			}
			certs = &atomic.Pointer[certStore]{}
			certs.Store(store)
			srv.TLSConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					return certs.Load().get(hello)
				},
			}
		}
		s.servers = append(s.servers, srv)
		s.certs = append(s.certs, certs)
	}
	return s, nil
}

// ReloadCerts loads the certificates of listeners, the listeners New was
// given with new certificate pairs, and returns a func that swaps them in.
// Nothing changes unless every certificate loads, and handshakes already
// under way finish with the certificate they picked. A listener whose
// address or TLS setting differs from New's is skipped: it needs a restart.
func (s *Server) ReloadCerts(listeners []config.Listener) (swap func(), err error) {
	stores := make([]*certStore, len(s.certs))
	for i, l := range listeners {
		if i >= len(s.certs) || s.certs[i] == nil || l.TLS == nil || l.Addr != s.servers[i].Addr {
			continue
		}
		if stores[i], err = loadCerts(l.TLS.Certs); err != nil {
			return nil, fmt.Errorf("listeners[%d]: %w", i, err)
		}
	}
	return func() {
		for i, store := range stores {
			if store != nil {
				s.certs[i].Store(store)
			}
		}
	}, nil
}

// Admin adds a listener on addr that serves h, the admin API. It skips the
// access log and the ready path, and shuts down with the other listeners.
// It must be called at most once, before Start.
func (s *Server) Admin(addr string, h http.Handler) {
	s.servers = append(s.servers, &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	})
	s.errc = make(chan error, len(s.servers))
	s.admin = true
}

// Start binds every listener, then serves them in the background. Binding
// all of them first means a port that is in use fails Start before any
// traffic is accepted. Errors from serving arrive on Err.
func (s *Server) Start() error {
	lns := make([]net.Listener, 0, len(s.servers))
	for _, srv := range s.servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return err
		}
		lns = append(lns, ln)
		s.addrs = append(s.addrs, ln.Addr())
	}
	for i, srv := range s.servers {
		isTLS := srv.TLSConfig != nil
		if s.admin && i == len(s.servers)-1 {
			s.log.Info("admin API listening", "addr", lns[i].Addr().String())
		} else {
			s.log.Info("listening", "addr", lns[i].Addr().String(), "tls", isTLS)
		}
		go func() {
			var err error
			if isTLS {
				// The certificates come from TLSConfig. ServeTLS also turns
				// on HTTP/2.
				err = srv.ServeTLS(lns[i], "", "")
			} else {
				err = srv.Serve(lns[i])
			}
			if !errors.Is(err, http.ErrServerClosed) {
				s.errc <- fmt.Errorf("listener %s: %w", srv.Addr, err)
			}
		}()
	}
	return nil
}

// Addrs returns the bound addresses, in listener order and then the admin
// API's, after Start.
func (s *Server) Addrs() []net.Addr { return s.addrs }

// AdminAddr returns the admin API's bound address after Start, or nil when
// there is none.
func (s *Server) AdminAddr() net.Addr {
	if !s.admin || len(s.addrs) == 0 {
		return nil
	}
	return s.addrs[len(s.addrs)-1]
}

// Err delivers errors that stop a listener from serving.
func (s *Server) Err() <-chan error { return s.errc }

// readiness answers readyPath ahead of next, so no route can shadow it. It
// skips the access log, which load balancers probing every second would
// otherwise fill.
func (s *Server) readiness(readyPath string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readyPath {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if s.draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ready\n")
	})
}

// Drain starts a graceful shutdown while still serving: the ready path
// answers 503, so load balancers in front stop sending new clients, and
// HTTP/1 responses carry "Connection: close", so clients with keep-alive
// connections reconnect elsewhere.
func (s *Server) Drain() {
	s.draining.Store(true)
	for _, srv := range s.servers {
		srv.SetKeepAlivesEnabled(false)
	}
}

// TrackHijacked registers a connection taken over from net/http, such as a
// WebSocket, which http.Server.Shutdown neither waits for nor closes.
// Shutdown waits for it like an active request and closes it at the
// deadline. The returned func must be called once the connection is done.
func (s *Server) TrackHijacked(c net.Conn) (untrack func()) {
	s.hijacked.add(c)
	return func() { s.hijacked.remove(c) }
}

// Shutdown stops accepting connections and waits until ctx is done for
// active requests and hijacked connections, then closes whatever is left.
func (s *Server) Shutdown(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, srv := range s.servers {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				srv.Close()
				mu.Lock()
				errs = append(errs, fmt.Errorf("listener %s: %w", srv.Addr, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if n, err := s.hijacked.drain(ctx); err != nil {
		errs = append(errs, fmt.Errorf("closed %d hijacked connections: %w", n, err))
	}
	return errors.Join(errs...)
}

// connSet is the set of hijacked connections.
type connSet struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (cs *connSet) add(c net.Conn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.conns == nil {
		cs.conns = make(map[net.Conn]struct{})
	}
	cs.conns[c] = struct{}{}
}

func (cs *connSet) remove(c net.Conn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.conns, c)
}

func (cs *connSet) len() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.conns)
}

// drainPoll is how often drain checks whether the set has emptied, as
// http.Server.Shutdown polls for idle connections.
const drainPoll = 10 * time.Millisecond

// drain waits for every connection to be removed. If ctx ends first, it
// closes and forgets the rest, and returns how many, with ctx's error.
func (cs *connSet) drain(ctx context.Context) (int, error) {
	t := time.NewTicker(drainPoll)
	defer t.Stop()
	for cs.len() > 0 {
		select {
		case <-t.C:
		case <-ctx.Done():
			cs.mu.Lock()
			defer cs.mu.Unlock()
			n := len(cs.conns)
			for c := range cs.conns {
				c.Close()
			}
			clear(cs.conns)
			return n, ctx.Err()
		}
	}
	return 0, nil
}
