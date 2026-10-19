// Package ratelimit limits request rates per client with the generic cell
// rate algorithm (GCRA), keeping one timestamp per client in a sharded map.
package ratelimit

import (
	"math"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"
)

const (
	shards = 64
	// sweepEvery is how many new keys a shard takes between sweeps for
	// idle keys, so the cost of a sweep is spread over the inserts.
	sweepEvery = 1024
	// shardCap bounds the keys in one shard, about 16 MB for the whole
	// limiter, however many distinct clients arrive within one burst
	// window.
	shardCap = 4096
)

// Limiter allows each key Count requests per Per on average, with bursts of
// up to Burst requests.
//
// GCRA stores one theoretical arrival time (TAT) per key: the time at which
// the key's bucket would be full again. A request at now moves it to
// max(TAT, now)+T, where T is the interval between requests at the steady
// rate, and is allowed if that is at most Burst×T ahead of now. It is
// exactly a token bucket of Burst tokens refilled every T, in one integer
// instead of a token count plus a timestamp.
//
// A key whose TAT has passed is the same as one never seen, so idle keys can
// be dropped at any time, which keeps memory bounded.
type Limiter struct {
	interval int64 // T, in nanoseconds
	limit    int64 // Burst × T: how far ahead of now a TAT may get
	seed     uint64
	now      func() int64 // monotonic nanoseconds; swapped in tests
	shards   [shards]shard
}

type shard struct {
	mu      sync.Mutex
	tat     map[[16]byte]int64 // no pointers, so the GC does not scan it
	inserts int
	_       [40]byte // keep shards on separate cache lines
}

// New returns a Limiter allowing count requests per per, with bursts of up
// to burst requests. count and burst must be at least 1.
func New(count int, per time.Duration, burst int) *Limiter {
	start := time.Now()
	l := &Limiter{
		interval: max(int64(per)/int64(count), 1),
		seed:     rand.Uint64(),
		now:      func() int64 { return int64(time.Since(start)) },
	}
	l.limit = math.MaxInt64 / 4 // a burst too large to matter
	if int64(burst) < l.limit/l.interval {
		l.limit = int64(burst) * l.interval
	}
	for i := range l.shards {
		l.shards[i].tat = make(map[[16]byte]int64)
	}
	return l
}

// Allow reports whether a request from key may go ahead. If not, it also
// returns how long until one would be allowed. A rejected request does not
// count against key.
func (l *Limiter) Allow(key netip.Addr) (ok bool, retryAfter time.Duration) {
	k := key.As16() // a zero Addr is all zeros: non-IP peers share a key
	s := &l.shards[l.shardOf(k)]
	now := l.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	tat, seen := s.tat[k]
	next := max(tat, now) + l.interval
	if wait := next - now - l.limit; wait > 0 {
		return false, time.Duration(wait)
	}
	if !seen {
		s.makeRoom(now)
	}
	s.tat[k] = next
	return true, 0
}

// makeRoom runs before a new key is added. Every sweepEvery inserts it drops
// the keys whose TAT has passed. If the shard is full in between, it drops
// an arbitrary key, which only gives that client a fresh burst; sweeping on
// every insert of a full shard would cost a full scan per new client.
func (s *shard) makeRoom(now int64) {
	s.inserts++
	if s.inserts >= sweepEvery {
		s.inserts = 0
		s.sweep(now)
	}
	if len(s.tat) >= shardCap {
		for k := range s.tat {
			delete(s.tat, k)
			break
		}
	}
}

// sweep drops idle keys. Go maps never shrink, so when most keys are idle
// it copies the live ones into a new map instead, giving the memory of a
// past flood back.
func (s *shard) sweep(now int64) {
	live := 0
	for _, tat := range s.tat {
		if tat > now {
			live++
		}
	}
	if live < len(s.tat)/2 {
		m := make(map[[16]byte]int64, live)
		for k, tat := range s.tat {
			if tat > now {
				m[k] = tat
			}
		}
		s.tat = m
		return
	}
	for k, tat := range s.tat {
		if tat <= now {
			delete(s.tat, k)
		}
	}
}

// shardOf mixes the key with a per-process seed, so clients cannot choose
// addresses that all land in one shard.
func (l *Limiter) shardOf(k [16]byte) int {
	var hi, lo uint64
	for i := range 8 {
		hi = hi<<8 | uint64(k[i])
		lo = lo<<8 | uint64(k[8+i])
	}
	z := hi ^ (lo * 0x9e3779b97f4a7c15) ^ l.seed
	z ^= z >> 30
	z *= 0xbf58476d1ce4e5b9
	z ^= z >> 27
	z *= 0x94d049bb133111eb
	z ^= z >> 31
	return int(z % shards)
}

// keys returns the number of keys held, for tests.
func (l *Limiter) keys() int {
	n := 0
	for i := range l.shards {
		s := &l.shards[i]
		s.mu.Lock()
		n += len(s.tat)
		s.mu.Unlock()
	}
	return n
}
