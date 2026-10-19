package upstream

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
)

// tcpKeepAlive is the TCP keep-alive probe interval on backend connections,
// so dead peers are noticed on idle pooled connections.
const tcpKeepAlive = 30 * time.Second

// newTransport builds the transport for one backend.
//
// Backends are spoken to over HTTP/1.1 only, as nginx does by default. That
// keeps Upgrade (WebSocket) requests simple and makes one request use one
// connection, which is what MaxIdleConnsPerHost and least-conn assume.
func newTransport(u *config.Upstream, target *url.URL, tc *config.BackendTLS) (*http.Transport, error) {
	dialer := &net.Dialer{Timeout: u.Timeouts.Connect, KeepAlive: tcpKeepAlive}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	t := &http.Transport{
		Protocols: &protocols,
		// Never pick up HTTP_PROXY and friends from the environment.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConnsPerHost:   u.MaxIdleConnsPerHost,
		IdleConnTimeout:       u.Timeouts.IdleConn,
		ResponseHeaderTimeout: u.Timeouts.ResponseHeader,
		TLSHandshakeTimeout:   u.Timeouts.Connect,
		ExpectContinueTimeout: time.Second,
		// Pass Accept-Encoding and compressed bodies through untouched
		// instead of letting the transport add gzip and decode it.
		DisableCompression: true,
	}
	if target.Scheme == "https" {
		cfg, err := clientTLSConfig(tc)
		if err != nil {
			return nil, err
		}
		t.TLSClientConfig = cfg
	}
	return t, nil
}

// clientTLSConfig verifies backends against the system roots, or only
// against ca_file when it is set. server_name overrides the name sent in SNI
// and checked in the certificate, which is needed when the backend URL is an
// IP address.
func clientTLSConfig(tc *config.BackendTLS) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if tc == nil {
		return cfg, nil
	}
	cfg.ServerName = tc.ServerName
	if tc.CAFile != "" {
		pem, err := os.ReadFile(tc.CAFile)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: %w", tc.CAFile, errNoCerts)
		}
		cfg.RootCAs = roots
	}
	return cfg, nil
}

var errNoCerts = errors.New("no PEM certificates found")
