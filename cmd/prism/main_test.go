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

// TestRunValidateAdminAuth checks that -validate refuses an admin API on a
// non-loopback address without admin.auth, and accepts it with auth.
func TestRunValidateAdminAuth(t *testing.T) {
	const base = `
listeners: [{ addr: ":8080" }]
upstreams:
  web: { backends: [{ url: "http://127.0.0.1:9000" }] }
routes:
  - { match: { path_prefix: / }, upstream: web }
`
	for _, tt := range []struct {
		admin string
		code  int
	}{
		{`admin: { listen: "0.0.0.0:9901" }`, 1},
		{`admin: { listen: "0.0.0.0:9901", auth: { api_keys: { header: Authorization, keys: { prometheus: "sha256:9b346041bc9a49574eb2665b2ad2a0a3f9f9cce4e42f5d1f26deb8a256b5966a" } } } }`, 0},
	} {
		cfgPath := filepath.Join(t.TempDir(), "prism.yaml")
		if err := os.WriteFile(cfgPath, []byte(base+tt.admin+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-config", cfgPath, "-validate"}, &stdout, &stderr); code != tt.code {
			t.Errorf("%s: exit code %d, want %d; stderr %q", tt.admin, code, tt.code, stderr.String())
		}
		if tt.code == 1 && !strings.Contains(stderr.String(), "admin.auth") {
			t.Errorf("stderr = %q, want admin.auth named", stderr.String())
		}
	}
}

func TestRunLogBuffer(t *testing.T) {
	tests := []struct {
		value string
		code  int
	}{
		{"64KB", 1}, // valid flag; the missing config is reported through the buffer
		{"0", 1},
		{"lots", 2},
		{"1GB", 2},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"-log-buffer", tt.value, "-config", "does-not-exist.yaml", "-validate"}, &stdout, &stderr)
			if code != tt.code {
				t.Fatalf("exit code %d, want %d, stderr:\n%s", code, tt.code, stderr.String())
			}
			if tt.code == 1 && !strings.Contains(stderr.String(), "no such file") {
				t.Errorf("stderr = %q, want the missing-file error flushed at exit", stderr.String())
			}
		})
	}
}
