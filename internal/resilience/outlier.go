package resilience

import (
	"sync"
	"sync/atomic"
	"time"
)

// OutlierConfig sets when an Outlier ejects a member and for how long.
type OutlierConfig struct {
	ConsecutiveFailures int
	BaseEjection        time.Duration
	MaxEjection         time.Duration
	MaxEjectionPercent  int
}

// Outlier ejects the members of a pool that fail ConsecutiveFailures times
// in a row. The n-th ejection in a row lasts min(BaseEjection*n, MaxEjection),
// and n goes down by one for every BaseEjection a member spends back in the
// pool, so a backend that keeps failing stays out longer each time while one
// that recovered is forgiven.
//
// An ejection that would take more than MaxEjectionPercent of the pool out
// at once is skipped. With the default of 50%, a pool of one is never
// ejected and a pool of two keeps at least one member.
//
// Members are numbered from 0. Ejected and the success path of Report are
// lock-free; deciding an ejection takes a mutex.
type Outlier struct {
	cfg     OutlierConfig
	origin  time.Time
	now     func() time.Time
	members []member

	mu sync.Mutex
}

type member struct {
	failures atomic.Int32
	// until is when the current ejection ends, in nanoseconds since origin,
	// and 0 when the member is not ejected. Ejected clears it lazily.
	until atomic.Int64

	// Under mu.
	ejections int   // n in BaseEjection*n
	lastEnd   int64 // when the last ejection ended, for decaying ejections

	_ [32]byte // pad to 64 bytes so neighbors' hot counters don't share a line
}

// NewOutlier returns a detector for a pool of n members, none ejected.
func NewOutlier(n int, cfg OutlierConfig) *Outlier {
	return &Outlier{cfg: cfg, origin: time.Now(), now: time.Now, members: make([]member, n)}
}

// Ejected reports whether member i is ejected.
func (o *Outlier) Ejected(i int) bool {
	m := &o.members[i]
	u := m.until.Load()
	if u == 0 {
		return false
	}
	if o.since() < u {
		return true
	}
	// Expired. Clearing it keeps later calls off the clock; losing the race
	// to a new ejection is fine, since the CAS then fails.
	m.until.CompareAndSwap(u, 0)
	return false
}

// Report records the outcome of a request to member i. It returns how long
// the member was ejected for when this report ejected it, and 0 otherwise.
func (o *Outlier) Report(i int, out Outcome) time.Duration {
	m := &o.members[i]
	switch out {
	case Success:
		// Load first, so a healthy member's counter is only read, not
		// written, on every request.
		if m.failures.Load() != 0 {
			m.failures.Store(0)
		}
	case Failure:
		if int(m.failures.Add(1)) >= o.cfg.ConsecutiveFailures {
			return o.eject(i)
		}
	}
	return 0
}

func (o *Outlier) eject(i int) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := &o.members[i]
	// Another failure's report may have ejected it already. Failures of
	// requests still in flight when it was ejected land here too.
	if int(m.failures.Load()) < o.cfg.ConsecutiveFailures || o.Ejected(i) {
		return 0
	}
	// Whether ejected or not, the member needs a new streak before the next
	// try, so a member held in by the cap doesn't take the lock on every
	// failure.
	m.failures.Store(0)

	ejected := 0
	for j := range o.members {
		if j != i && o.Ejected(j) {
			ejected++
		}
	}
	if (ejected+1)*100 > o.cfg.MaxEjectionPercent*len(o.members) {
		return 0
	}

	now := o.since()
	if m.ejections > 0 {
		clean := (now - m.lastEnd) / int64(o.cfg.BaseEjection)
		m.ejections = max(0, m.ejections-int(clean))
	}
	m.ejections++
	d := min(o.cfg.BaseEjection*time.Duration(m.ejections), o.cfg.MaxEjection)
	m.lastEnd = now + int64(d)
	m.until.Store(m.lastEnd)
	return d
}

func (o *Outlier) since() int64 { return int64(o.now().Sub(o.origin)) }
