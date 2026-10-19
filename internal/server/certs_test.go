package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
)

// writeCert writes a self-signed certificate for dnsNames, and its key, to
// dir. It returns the pair and the parsed certificate, which can serve as its
// own root in a client.
func writeCert(t testing.TB, dir, name string, dnsNames ...string) (config.CertPair, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              dnsNames,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair := config.CertPair{Cert: filepath.Join(dir, name+".pem"), Key: filepath.Join(dir, name+".key")}
	writePEM(t, pair.Cert, "CERTIFICATE", der)
	writePEM(t, pair.Key, "PRIVATE KEY", keyDER)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pair, cert
}

func writePEM(t testing.TB, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCertStore(t *testing.T) {
	dir := t.TempDir()
	a, _ := writeCert(t, dir, "a", "a.test", "www.a.test")
	w, _ := writeCert(t, dir, "w", "*.wild.test")
	b, _ := writeCert(t, dir, "b", "b.test", "a.test") // a.test is a's: first listed wins
	s, err := loadCerts([]config.CertPair{a, w, b})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct{ sni, want string }{
		{"a.test", "a"},
		{"A.Test.", "a"},
		{"www.a.test", "a"},
		{"b.test", "b"},
		{"x.wild.test", "w"},
		{"y.x.wild.test", "a"}, // a wildcard covers one label only
		{"wild.test", "a"},
		{"unknown.test", "a"},
		{"", "a"},
	}
	for _, tt := range tests {
		c, err := s.get(&tls.ClientHelloInfo{ServerName: tt.sni})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Leaf.Subject.CommonName; got != tt.want {
			t.Errorf("SNI %q: got certificate %q, want %q", tt.sni, got, tt.want)
		}
	}
}

func TestLoadCertsErrors(t *testing.T) {
	dir := t.TempDir()
	a, _ := writeCert(t, dir, "a", "a.test")
	b, _ := writeCert(t, dir, "b", "b.test")
	noNames, _ := writeCert(t, dir, "nonames")

	tests := []struct {
		name  string
		pairs []config.CertPair
		want  string
	}{
		{"missing file", []config.CertPair{{Cert: filepath.Join(dir, "nope.pem"), Key: a.Key}}, "nope.pem"},
		{"key does not match", []config.CertPair{{Cert: a.Cert, Key: b.Key}}, "private key does not match"},
		{"no DNS names", []config.CertPair{noNames}, "no DNS names"},
		{"none", nil, "no certificates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadCerts(tt.pairs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func BenchmarkCertGet(b *testing.B) {
	dir := b.TempDir()
	a, _ := writeCert(b, dir, "a", "a.test")
	w, _ := writeCert(b, dir, "w", "*.wild.test")
	s, err := loadCerts([]config.CertPair{a, w})
	if err != nil {
		b.Fatal(err)
	}
	hello := &tls.ClientHelloInfo{ServerName: "x.wild.test"}
	b.ReportAllocs()
	for b.Loop() {
		s.get(hello)
	}
}
