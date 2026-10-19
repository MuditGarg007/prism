package main

import (
	"bytes"
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
