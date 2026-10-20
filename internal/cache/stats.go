package cache

import "sync/atomic"

// Stats counts how cached routes answered requests, one counter per X-Cache
// value. Every route shares one Stats. It is safe for concurrent use.
type Stats struct {
	Hit         atomic.Uint64 // served from a fresh entry
	Miss        atomic.Uint64 // fetched from a backend
	Stale       atomic.Uint64 // served past freshness because the backend failed
	Revalidated atomic.Uint64 // a stale entry the backend confirmed with a 304
	Bypass      atomic.Uint64 // the bypass header skipped the lookup
}

// StatsSnapshot is a copy of Stats at one moment.
type StatsSnapshot struct {
	Hit, Miss, Stale, Revalidated, Bypass uint64
}

// Snapshot reads each counter. They are read one at a time, so a snapshot
// taken under load may be off by the requests that finished meanwhile.
func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Hit:         s.Hit.Load(),
		Miss:        s.Miss.Load(),
		Stale:       s.Stale.Load(),
		Revalidated: s.Revalidated.Load(),
		Bypass:      s.Bypass.Load(),
	}
}
