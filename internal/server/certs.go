package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	"github.com/MuditGarg007/prism/internal/config"
)

// certStore picks the certificate for a TLS handshake by SNI name.
type certStore struct {
	exact    map[string]*tls.Certificate
	wildcard map[string]*tls.Certificate // keyed by suffix with its leading dot, ".example.com"
	fallback *tls.Certificate
}

// loadCerts loads every pair and indexes it by the DNS names in its leaf
// certificate. When two certificates share a name, the first one listed
// wins. The first certificate is also the fallback for unknown names and for
// clients that send no SNI.
func loadCerts(pairs []config.CertPair) (*certStore, error) {
	s := &certStore{
		exact:    map[string]*tls.Certificate{},
		wildcard: map[string]*tls.Certificate{},
	}
	for _, p := range pairs {
		cert, err := tls.LoadX509KeyPair(p.Cert, p.Key)
		if err != nil {
			return nil, fmt.Errorf("certificate %s: %w", p.Cert, err)
		}
		if cert.Leaf == nil {
			// Only when GODEBUG x509keypairleaf=0 turns the parsing off.
			if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
				return nil, fmt.Errorf("certificate %s: %w", p.Cert, err)
			}
		}
		if len(cert.Leaf.DNSNames) == 0 {
			return nil, fmt.Errorf("certificate %s: %w", p.Cert, errNoDNSNames)
		}
		for _, name := range cert.Leaf.DNSNames {
			name = strings.ToLower(name)
			m, key := s.exact, name
			if suffix, ok := strings.CutPrefix(name, "*"); ok {
				m, key = s.wildcard, suffix
			}
			if _, dup := m[key]; !dup {
				m[key] = &cert
			}
		}
		if s.fallback == nil {
			s.fallback = &cert
		}
	}
	if s.fallback == nil {
		return nil, errors.New("no certificates")
	}
	return s, nil
}

var errNoDNSNames = errors.New("no DNS names in the subject alternative names")

// get is the tls.Config GetCertificate callback. It tries the exact name,
// then a wildcard. A wildcard covers exactly one label, as RFC 6125 says, so
// "*.example.com" matches "a.example.com" but not "a.b.example.com"; that is
// stricter than route matching, because a client would reject the
// certificate anyway.
func (s *certStore) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if c, ok := s.exact[name]; ok {
		return c, nil
	}
	if i := strings.IndexByte(name, '.'); i > 0 {
		if c, ok := s.wildcard[name[i:]]; ok {
			return c, nil
		}
	}
	return s.fallback, nil
}
