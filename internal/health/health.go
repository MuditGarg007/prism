// Package health actively checks the backends of a pool and marks them
// healthy or unhealthy.
package health

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/MuditGarg007/prism/internal/upstream"
)

// drainLimit is how much of a check's response body is read so that its
// connection can be reused. A longer body closes the connection instead.
const drainLimit = 4 << 10

// Config says how to check a backend.
type Config struct {
	Path           string // requested with GET; may carry a query
	Interval       time.Duration
	Timeout        time.Duration
	UnhealthyAfter int // consecutive failed checks that mark a backend down
	HealthyAfter   int // consecutive passed checks that mark it up again
}

// Checker runs one goroutine per backend of a pool. New backends start
// healthy, so a new pool serves traffic before its first round of checks.
type Checker struct {
	cfg  Config
	pool *upstream.Pool
	log  *slog.Logger

	// randN returns a random duration in [0, n). Tests replace it.
	randN func(n time.Duration) time.Duration

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New returns a Checker for every backend of pool. Nothing runs until Start.
func New(pool *upstream.Pool, cfg Config, log *slog.Logger) *Checker {
	return &Checker{
		cfg:   cfg,
		pool:  pool,
		log:   log.With("upstream", pool.Name),
		randN: rand.N[time.Duration],
	}
}

// Start starts checking. It must be called at most once.
func (c *Checker) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	for _, b := range c.pool.Backends {
		c.wg.Go(func() { c.run(ctx, b) })
	}
}

// Stop stops checking and waits for checks in progress to end. It is safe to
// call more than once, and without Start.
func (c *Checker) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
}

func (c *Checker) run(ctx context.Context, b *upstream.Backend) {
	log := c.log.With("backend", b.String())
	// Start from the backend's current state, which a reload carries over,
	// so a backend that was down needs healthy_after passes to come back.
	t := tracker{unhealthyAfter: c.cfg.UnhealthyAfter, healthyAfter: c.cfg.HealthyAfter, unhealthy: !b.Healthy()}
	// A random first offset spreads the checks of many backends, and of
	// many proxies started together, over the interval.
	timer := time.NewTimer(c.randN(c.cfg.Interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		err := c.check(ctx, b)
		if ctx.Err() != nil {
			return // stopped mid-check; the result says nothing
		}
		if err != nil {
			log.Debug("health check failed", "error", err)
		}
		if t.observe(err == nil) {
			b.SetHealthy(!t.unhealthy)
			if t.unhealthy {
				log.Warn("backend marked unhealthy", "checks", c.cfg.UnhealthyAfter, "error", err)
			} else {
				log.Info("backend marked healthy", "checks", c.cfg.HealthyAfter)
			}
		}
		timer.Reset(c.next())
	}
}

// next returns the wait before the next check: the interval ±10%, so checks
// that started together drift apart.
func (c *Checker) next() time.Duration {
	spread := c.cfg.Interval / 5
	if spread <= 0 {
		return c.cfg.Interval
	}
	return c.cfg.Interval - spread/2 + c.randN(spread)
}

// check sends one GET to b through its own transport, so it uses b's TLS
// settings and pooled connections. Any 2xx or 3xx passes.
func (c *Checker) check(ctx context.Context, b *upstream.Backend) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.Scheme+"://"+b.URL.Host+c.cfg.Path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "prism-health-check")
	resp, err := b.Transport.RoundTrip(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// tracker turns check results into a health state. The state flips only
// after a streak of results that disagree with it, so one lost check never
// takes a backend out, and one lucky check never brings it back.
type tracker struct {
	unhealthyAfter, healthyAfter int

	unhealthy bool
	streak    int // consecutive results that disagree with the state
}

// observe records one result and reports whether the state flipped.
func (t *tracker) observe(ok bool) bool {
	if ok != t.unhealthy {
		t.streak = 0
		return false
	}
	t.streak++
	need := t.unhealthyAfter
	if t.unhealthy {
		need = t.healthyAfter
	}
	if t.streak < need {
		return false
	}
	t.unhealthy, t.streak = !t.unhealthy, 0
	return true
}
