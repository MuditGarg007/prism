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

// transportConfig is everything a backend's transport is built from. A
// reload keeps a backend's transport, and so its pooled connections, only
// while this is equal. The CA file's contents are part of it, so a rotated
// CA takes effect on reload.
type transportConfig struct {
	connect, responseHeader, idleConn time.Duration
	maxIdleConnsPerHost               int

	https      bool
	serverName string
	caFile     string
	caPEM      string
}

func newTransportConfig(u *config.Upstream, target *url.URL, tc *config.BackendTLS) (transportConfig, error) {
	c := transportConfig{
		connect:             u.Timeouts.Connect,
		responseHeader:      u.Timeouts.ResponseHeader,
		idleConn:            u.Timeouts.IdleConn,
		maxIdleConnsPerHost: u.MaxIdleConnsPerHost,
		https:               target.Scheme == "https",
	}
	if c.https && tc != nil {
		c.serverName, c.caFile = tc.ServerName, tc.CAFile
		if tc.CAFile != "" {
			pem, err := os.ReadFile(tc.CAFile)
			if err != nil {
				return transportConfig{}, err
			}
			c.caPEM = string(pem)
		}
	}
	return c, nil
}

// newTransport builds the transport for one backend.
//
// Backends are spoken to over HTTP/1.1 only, as nginx does by default. That
// keeps Upgrade (WebSocket) requests simple and makes one request use one
// connection, which is what MaxIdleConnsPerHost and least-conn assume.
func newTransport(c transportConfig) (*http.Transport, error) {
	dialer := &net.Dialer{Timeout: c.connect, KeepAlive: tcpKeepAlive}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	t := &http.Transport{
		Protocols: &protocols,
		// Never pick up HTTP_PROXY and friends from the environment.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConnsPerHost:   c.maxIdleConnsPerHost,
		IdleConnTimeout:       c.idleConn,
		ResponseHeaderTimeout: c.responseHeader,
		TLSHandshakeTimeout:   c.connect,
		ExpectContinueTimeout: time.Second,
		// Pass Accept-Encoding and compressed bodies through untouched
		// instead of letting the transport add gzip and decode it.
		DisableCompression: true,
	}
	if c.https {
		cfg, err := clientTLSConfig(c)
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
func clientTLSConfig(c transportConfig) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.serverName}
	if c.caFile != "" {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(c.caPEM)) {
			return nil, fmt.Errorf("ca_file %s: %w", c.caFile, errNoCerts)
		}
		cfg.RootCAs = roots
	}
	return cfg, nil
}

var errNoCerts = errors.New("no PEM certificates found")
