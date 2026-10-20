package resilience

import (
	"testing"
	"time"
)

func TestRetryable(t *testing.T) {
	p, err := NewRetry(2, []string{"connect_error", "timeout", "502", "503"})
	if err != nil {
		t.Fatal(err)
	}
	connectOnly, err := NewRetry(1, []string{"connect_error"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		p          *Retry
		a          Attempt
		idempotent bool
		want       bool
	}{
		{"connect, idempotent", p, Attempt{Connect: true}, true, true},
		{"connect, not idempotent", p, Attempt{Connect: true}, false, true},
		{"connect timeout, not idempotent", p, Attempt{Connect: true, Timeout: true}, false, true},
		{"header timeout, idempotent", p, Attempt{Timeout: true}, true, true},
		{"header timeout, not idempotent", p, Attempt{Timeout: true}, false, false},
		{"listed status, idempotent", p, Attempt{Status: 503}, true, true},
		{"listed status, not idempotent", p, Attempt{Status: 503}, false, false},
		{"unlisted 5xx", p, Attempt{Status: 500}, true, false},
		{"4xx", p, Attempt{Status: 404}, true, false},
		{"2xx", p, Attempt{Status: 200}, true, false},
		{"other error", p, Attempt{}, true, false},
		{"timeout not listed", connectOnly, Attempt{Timeout: true}, true, false},
		{"status not listed", connectOnly, Attempt{Status: 502}, true, false},
		{"connect timeout counts as connect", connectOnly, Attempt{Connect: true, Timeout: true}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Retryable(tt.a, tt.idempotent); got != tt.want {
				t.Errorf("Retryable(%+v, idempotent=%v) = %v, want %v", tt.a, tt.idempotent, got, tt.want)
			}
		})
	}
}

// TestRetryStatusBits checks every 5xx code against the bitset, across both
// words.
func TestRetryStatusBits(t *testing.T) {
	p, err := NewRetry(1, []string{"500", "563", "564", "599"})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int]bool{500: true, 563: true, 564: true, 599: true}
	for s := 400; s < 700; s++ {
		if got := p.Retryable(Attempt{Status: s}, true); got != listed[s] {
			t.Errorf("status %d: Retryable = %v, want %v", s, got, listed[s])
		}
	}
}

func TestNewRetryRejects(t *testing.T) {
	for _, cond := range []string{"404", "600", "5xx", "", "error"} {
		if _, err := NewRetry(1, []string{cond}); err == nil {
			t.Errorf("NewRetry accepted condition %q", cond)
		}
	}
}

func TestIdempotent(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS", "TRACE", "PUT", "DELETE"} {
		if !Idempotent(m) {
			t.Errorf("Idempotent(%s) = false", m)
		}
	}
	for _, m := range []string{"POST", "PATCH", "CONNECT", "get", ""} {
		if Idempotent(m) {
			t.Errorf("Idempotent(%q) = true", m)
		}
	}
}

func TestRetryableAllocs(t *testing.T) {
	p, _ := NewRetry(1, []string{"503"})
	allocs := testing.AllocsPerRun(100, func() { p.Retryable(Attempt{Status: 503}, Idempotent("GET")) })
	if allocs != 0 {
		t.Errorf("Retryable allocates %v times, want 0", allocs)
	}
}

func TestDelay(t *testing.T) {
	p, err := NewRetry(3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := p.Delay(0); d != 0 {
		t.Errorf("zero Backoff waits %v", d)
	}
	p.Backoff = Backoff{Base: 25 * time.Millisecond, Max: 250 * time.Millisecond}
	var ceil time.Duration
	p.randN = func(n time.Duration) time.Duration { ceil = n; return n / 2 }
	for n, want := range map[int]time.Duration{
		0:   25 * time.Millisecond,
		1:   50 * time.Millisecond,
		3:   200 * time.Millisecond,
		4:   250 * time.Millisecond, // 400 ms, capped
		62:  250 * time.Millisecond,
		100: 250 * time.Millisecond, // no overflow
	} {
		if d := p.Delay(n); ceil != want || d != want/2 {
			t.Errorf("Delay(%d) drew from [0, %v) and gave %v, want [0, %v)", n, ceil, d, want)
		}
	}

	// The real random source stays under the ceiling.
	p, _ = NewRetry(1, nil)
	p.Backoff = Backoff{Base: time.Millisecond, Max: time.Second}
	for range 1000 {
		if d := p.Delay(2); d < 0 || d >= 4*time.Millisecond {
			t.Fatalf("Delay(2) = %v, outside [0, 4ms)", d)
		}
	}
}
