package ratelimit

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a fake monotonic clock.
type clock struct{ t atomic.Int64 }

func (c *clock) now() int64              { return c.t.Load() }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }
func withClock(l *Limiter) (*Limiter, *clock) {
	c := &clock{}
	l.now = c.now
	return l, c
}

var (
	ipA = netip.MustParseAddr("192.0.2.1")
	ipB = netip.MustParseAddr("2001:db8::1")
)

// addr returns a distinct IPv4 address for each i.
func addr(i int) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(i))
	return netip.AddrFrom4(b)
}

func TestAllowSequence(t *testing.T) {
	type step struct {
		after time.Duration // clock advance before the request
		ok    bool
		retry time.Duration
	}
	ms := time.Millisecond
	for _, tt := range []struct {
		name         string
		count, burst int
		per          time.Duration
		steps        []step
	}{
		{
			name: "burst then steady rate", count: 10, per: time.Second, burst: 3,
			steps: []step{
				{0, true, 0}, {0, true, 0}, {0, true, 0},
				{0, false, 100 * ms},
				{40 * ms, false, 60 * ms}, // a rejected request does not count
				{60 * ms, true, 0},
				{0, false, 100 * ms},
			},
		},
		{
			name: "idle refills only up to the burst", count: 10, per: time.Second, burst: 2,
			steps: []step{
				{0, true, 0}, {0, true, 0},
				{time.Hour, true, 0}, {0, true, 0},
				{0, false, 100 * ms},
			},
		},
		{
			name: "burst of one is a strict interval", count: 2, per: time.Second, burst: 1,
			steps: []step{
				{0, true, 0},
				{499 * ms, false, ms},
				{ms, true, 0},
				{500 * ms, true, 0},
			},
		},
		{
			name: "per hour", count: 60, per: time.Hour, burst: 1,
			steps: []step{
				{0, true, 0},
				{30 * time.Second, false, 30 * time.Second},
				{30 * time.Second, true, 0},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l, clk := withClock(New(tt.count, tt.per, tt.burst))
			for i, s := range tt.steps {
				clk.advance(s.after)
				ok, retry := l.Allow(ipA)
				if ok != s.ok || retry != s.retry {
					t.Errorf("step %d: Allow = %v, %v; want %v, %v", i, ok, retry, s.ok, s.retry)
				}
			}
		})
	}
}

// TestRetryAfterIsExact checks that waiting the returned time is exactly
// enough: a nanosecond less is still rejected.
func TestRetryAfterIsExact(t *testing.T) {
	l, clk := withClock(New(3, time.Second, 2))
	l.Allow(ipA)
	l.Allow(ipA)
	ok, retry := l.Allow(ipA)
	if ok || retry <= 0 {
		t.Fatalf("Allow = %v, %v; want a rejection", ok, retry)
	}
	clk.advance(retry - 1)
	if ok, _ := l.Allow(ipA); ok {
		t.Fatal("allowed before the retry time")
	}
	clk.advance(1)
	if ok, _ := l.Allow(ipA); !ok {
		t.Fatal("rejected at the retry time")
	}
}

func TestSteadyRate(t *testing.T) {
	l, clk := withClock(New(100, time.Second, 1))
	for i := range 1000 {
		if ok, _ := l.Allow(ipA); !ok {
			t.Fatalf("request %d at exactly the rate was rejected", i)
		}
		clk.advance(10 * time.Millisecond)
	}
	// Twice the rate: every other request goes through.
	allowed := 0
	for range 1000 {
		if ok, _ := l.Allow(ipA); ok {
			allowed++
		}
		clk.advance(5 * time.Millisecond)
	}
	if allowed != 500 {
		t.Errorf("allowed %d of 1000 at twice the rate, want 500", allowed)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := withClock(New(1, time.Minute, 1))
	if ok, _ := l.Allow(ipA); !ok {
		t.Fatal("first request rejected")
	}
	if ok, _ := l.Allow(ipA); ok {
		t.Fatal("second request from the same key allowed")
	}
	for _, k := range []netip.Addr{ipB, addr(1), {}} {
		if ok, _ := l.Allow(k); !ok {
			t.Errorf("%v was limited by another key's requests", k)
		}
	}
}

// TestConcurrentBurst checks that racing requests from one key get exactly
// the burst between them, no more.
func TestConcurrentBurst(t *testing.T) {
	l, _ := withClock(New(100, time.Second, 50))
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 100 {
				if ok, _ := l.Allow(ipA); ok {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if n := allowed.Load(); n != 50 {
		t.Errorf("allowed %d, want the burst of 50", n)
	}
}

// TestFloodIsBounded sends a flood of unique clients whose entries would
// all stay live for an hour: memory stays capped, and the newest clients
// are still limited.
func TestFloodIsBounded(t *testing.T) {
	l, _ := withClock(New(1, time.Hour, 1))
	const n = 500000
	for i := range n {
		if ok, _ := l.Allow(addr(i)); !ok {
			t.Fatalf("client %d rejected on its first request", i)
		}
	}
	if got, max := l.keys(), shards*shardCap; got > max {
		t.Errorf("holding %d keys after a flood of %d, want at most %d", got, n, max)
	}
	if ok, _ := l.Allow(addr(n - 1)); ok {
		t.Error("the newest client was not limited")
	}
}

// TestIdleKeysAreSwept checks that keys whose bucket has refilled are
// dropped once new clients arrive.
func TestIdleKeysAreSwept(t *testing.T) {
	l, clk := withClock(New(10, time.Second, 1)) // a key is idle 100ms after its request
	const n = 100000
	for i := range n {
		l.Allow(addr(i))
	}
	if got := l.keys(); got != n {
		t.Fatalf("holding %d keys, want %d", got, n)
	}
	clk.advance(time.Second)
	for i := range n {
		l.Allow(addr(n + i))
	}
	// Without sweeping this would be 2n. Each shard sweeps at least once
	// during the second wave, dropping every key from the first.
	if got := l.keys(); got > n+shards*sweepEvery {
		t.Errorf("holding %d keys, want at most %d", got, n+shards*sweepEvery)
	}
}

func TestShardSpread(t *testing.T) {
	l := New(1, time.Second, 1)
	var counts [shards]int
	const n = 64000
	for i := range n {
		k := addr(i).As16()
		counts[l.shardOf(k)]++
	}
	for i, c := range counts {
		if c < n/shards*8/10 || c > n/shards*12/10 {
			t.Errorf("shard %d got %d of %d sequential addresses, want about %d", i, c, n, n/shards)
		}
	}
}

func TestAllowNoAllocs(t *testing.T) {
	l, _ := withClock(New(1000, time.Second, 1000))
	l.Allow(ipA)
	if n := testing.AllocsPerRun(100, func() { l.Allow(ipA) }); n != 0 {
		t.Errorf("Allow on a known key allocates %v times, want 0", n)
	}
}

// FuzzGCRA checks Allow against a token bucket that holds burst×T
// nanoseconds of credit and spends T per request; GCRA is that bucket
// stored as one timestamp, so the decisions and waits must match exactly.
func FuzzGCRA(f *testing.F) {
	f.Add(uint8(10), uint8(3), []byte{0, 0, 0, 0, 40, 60, 0})
	f.Add(uint8(1), uint8(1), []byte{255, 1, 2, 3})
	f.Fuzz(func(t *testing.T, count, burst uint8, gaps []byte) {
		if count == 0 || burst == 0 {
			return
		}
		l, clk := withClock(New(int(count), time.Second, int(burst)))
		interval := int64(time.Second) / int64(count)
		capacity := int64(burst) * interval
		credit, last := capacity, int64(0)
		for i, g := range gaps {
			clk.advance(time.Duration(g) * time.Millisecond)
			now := clk.now()
			credit = min(capacity, credit+now-last)
			last = now
			wantOK, wantWait := credit >= interval, time.Duration(0)
			if wantOK {
				credit -= interval
			} else {
				wantWait = time.Duration(interval - credit)
			}
			ok, wait := l.Allow(ipA)
			if ok != wantOK || wait != wantWait {
				t.Fatalf("request %d: Allow = %v, %v; token bucket says %v, %v", i, ok, wait, wantOK, wantWait)
			}
		}
	})
}

func BenchmarkAllow(b *testing.B) {
	// One hot key serializes on one shard's lock; many keys spread over the
	// shards. The gap between them is what sharding buys.
	// The rate keeps every request allowed and every key live, so neither
	// rejections nor sweeps blur the numbers.
	newBench := func() *Limiter { return New(1<<30, 1000*time.Hour, 1<<30) }
	b.Run("one key", func(b *testing.B) {
		l := newBench()
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				l.Allow(ipA)
			}
		})
	})
	b.Run("many keys", func(b *testing.B) {
		l := newBench()
		const keys = 1 << 16
		for i := range keys {
			l.Allow(addr(i))
		}
		var next atomic.Int64
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			i := int(next.Add(1)) * 7919
			for pb.Next() {
				l.Allow(addr(i % keys))
				i++
			}
		})
	})
}
