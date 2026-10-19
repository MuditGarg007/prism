// Package server runs the HTTP and HTTPS listeners that feed requests to the
// proxy.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/MuditGarg007/prism/internal/config"
)

// Server is one http.Server per configured listener, all serving the same
// handler.
type Server struct {
	log     *slog.Logger
	servers []*http.Server
	addrs   []net.Addr
	errc    chan error
}

// New prepares a server for each listener and loads their certificates.
// Nothing listens until Start.
func New(listeners []config.Listener, h http.Handler, log *slog.Logger) (*Server, error) {
	s := &Server{log: log, errc: make(chan error, len(listeners))}
	handler := accessLog(h, log)
	for i, l := range listeners {
		srv := &http.Server{
			Addr:              l.Addr,
			Handler:           handler,
			ReadHeaderTimeout: l.Timeouts.ReadHeader,
			IdleTimeout:       l.Timeouts.Idle,
			// Mostly TLS handshake failures from clients and scanners.
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
		if l.TLS != nil {
			certs, err := loadCerts(l.TLS.Certs)
			if err != nil {
				return nil, fmt.Errorf("listeners[%d]: %w", i, err)
			}
			srv.TLSConfig = &tls.Config{
				MinVersion:     tls.VersionTLS12,
				GetCertificate: certs.get,
			}
		}
		s.servers = append(s.servers, srv)
	}
	return s, nil
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
		s.log.Info("listening", "addr", lns[i].Addr().String(), "tls", isTLS)
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

// Addrs returns the bound addresses, in listener order, after Start.
func (s *Server) Addrs() []net.Addr { return s.addrs }

// Err delivers errors that stop a listener from serving.
func (s *Server) Err() <-chan error { return s.errc }

// Shutdown stops accepting connections and waits for active requests until
// ctx is done, then closes whatever is left. A full graceful drain comes in
// M3.
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
	return errors.Join(errs...)
}
