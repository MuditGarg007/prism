// Package resilience decides how the proxy reacts to failing backends: the
// retry policy, its budget and backoff, circuit breakers and outlier
// detection.
package resilience

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Retry is a route's retry policy: which failed attempts may be tried again
// on another backend, and how many times.
//
// It only judges the failure and the method. Whether the request body can
// be sent again, and whether an untried backend is left, are the caller's
// to check.
type Retry struct {
	Attempts int     // retries after the first try
	Backoff  Backoff // the zero Backoff retries at once
	Budget   *Budget // nil for no budget

	connect bool
	timeout bool
	status  [2]uint64 // bit s-500 is set for each 5xx status s to retry

	randN func(time.Duration) time.Duration // replaced by tests
}

// Backoff spreads retries out in time: retry n waits a random time in
// [0, min(Max, Base×2ⁿ)). This "full jitter" keeps clients that failed
// together from retrying together.
type Backoff struct {
	Base, Max time.Duration
}

// NewRetry returns a policy that retries up to attempts times on the
// conditions in on: "connect_error", "timeout" or a 5xx status code.
func NewRetry(attempts int, on []string) (*Retry, error) {
	p := &Retry{Attempts: attempts, randN: rand.N[time.Duration]}
	for _, cond := range on {
		switch cond {
		case "connect_error":
			p.connect = true
		case "timeout":
			p.timeout = true
		default:
			s, err := strconv.Atoi(cond)
			if err != nil || s < 500 || s > 599 {
				return nil, fmt.Errorf("unknown retry condition %q", cond)
			}
			s -= 500
			p.status[s/64] |= 1 << (s % 64)
		}
	}
	return p, nil
}

// Attempt is how one attempt to forward a request ended.
type Attempt struct {
	// Connect is set when no connection to the backend was made, including
	// when connecting timed out. The backend then saw nothing.
	Connect bool
	// Timeout is set when connecting or waiting for the response headers
	// timed out.
	Timeout bool
	// Status is the response status, or 0 if there was no response.
	Status int
}

// Retryable reports whether an attempt that ended as a did may be tried
// again. A failed connection is safe to retry for any method, because the
// backend never saw the request. A timeout or a 5xx means the backend may
// have acted on it, so those are retried only for idempotent methods.
func (p *Retry) Retryable(a Attempt, idempotent bool) bool {
	if a.Connect && p.connect {
		return true
	}
	if !idempotent {
		return false
	}
	if a.Timeout && p.timeout {
		return true
	}
	s := a.Status - 500
	return s >= 0 && s < 100 && p.status[s/64]&(1<<(s%64)) != 0
}

// Delay returns how long to wait before retry n, counted from 0.
func (p *Retry) Delay(n int) time.Duration {
	b := p.Backoff
	ceil := b.Max
	// Base<<n <= Max exactly when Base <= Max>>n, which cannot overflow.
	if n < 63 && b.Base <= b.Max>>n {
		ceil = b.Base << n
	}
	if ceil <= 0 {
		return 0
	}
	return p.randN(ceil)
}

// Idempotent reports whether sending a request with method twice has the
// same effect as sending it once (RFC 9110, section 9.2.2).
func Idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace,
		http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}
