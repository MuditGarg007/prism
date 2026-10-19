package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunValidate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", "../../examples/prism.yaml", "-validate"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "is valid") {
		t.Errorf("stdout = %q, want it to say the config is valid", stdout.String())
	}
}

func TestRunMissingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", "does-not-exist.yaml", "-validate"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such file") {
		t.Errorf("stderr = %q, want a missing-file error", stderr.String())
	}
}

func TestRunValidateBuildsRuntime(t *testing.T) {
	// The CA file exists, so config validation passes; only building the
	// transport finds that it holds no certificates.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "prism.yaml")
	cfg := `
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: "https://127.0.0.1:9443", tls: { ca_file: ca.pem } }] }
routes:
  - { match: { path_prefix: / }, upstream: web }
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", cfgPath, "-validate"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no PEM certificates") {
		t.Errorf("stderr = %q, want the bad CA file reported", stderr.String())
	}
}
