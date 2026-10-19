package balancer

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"net/url"
	"testing"

	"github.com/MuditGarg007/prism/internal/upstream"
)

// hosts returns backends with distinct URLs, which the ring hashes on.
func hosts(ws ...int) []*upstream.Backend {
	bs := withWeights(ws...)
	for i, b := range bs {
		b.URL = &url.URL{Scheme: "http", Host: fmt.Sprintf("10.0.0.%d:8080", i+1)}
	}
	return bs
}

func equal(n int) []int {
	ws := make([]int, n)
	for i := range ws {
		ws[i] = 1
	}
	return ws
}

// clientIPs returns n distinct, seeded addresses, half IPv4 and half IPv6.
func clientIPs(n int) []netip.Addr {
	r := rand.New(rand.NewPCG(3, 4))
	seen := make(map[netip.Addr]bool, n)
	ips := make([]netip.Addr, 0, n)
	for len(ips) < n {
		var a netip.Addr
		if len(ips)%2 == 0 {
			a = netip.AddrFrom4([4]byte{byte(r.Uint32()), byte(r.Uint32()), byte(r.Uint32()), byte(r.Uint32())})
		} else {
			var b [16]byte
			for i := range b {
				b[i] = byte(r.Uint32())
			}
			a = netip.AddrFrom16(b)
		}
		if !seen[a] {
			seen[a] = true
			ips = append(ips, a)
		}
	}
	return ips
}

// assign maps each IP to the index of the backend bal picks for it.
func assign(t *testing.T, bal upstream.Balancer, bs []*upstream.Backend, ips []netip.Addr) []int {
	t.Helper()
	got := make([]int, len(ips))
	for i, ip := range ips {
		b, err := bal.Pick(upstream.Choice{ClientIP: ip})
		if err != nil {
			t.Fatal(err)
		}
		got[i] = indexOf(bs, b)
	}
	return got
}

func TestIPHashSticky(t *testing.T) {
	bs := hosts(equal(5)...)
	ips := clientIPs(1000)
	first := assign(t, NewIPHash(bs), bs, ips)

	// The same balancer again, and a second instance built from equal
	// backends, as another Prism process would: both must agree.
	again := assign(t, NewIPHash(bs), bs, ips)
	other := hosts(equal(5)...)
	elsewhere := assign(t, NewIPHash(other), other, ips)
	for i := range ips {
		if again[i] != first[i] || elsewhere[i] != first[i] {
			t.Fatalf("%v went to %d, then %d, then %d on another instance", ips[i], first[i], again[i], elsewhere[i])
		}
	}
}

func TestIPHashSpread(t *testing.T) {
	for _, tt := range []struct {
		weights   []int
		tolerance float64 // allowed deviation from the expected share, relative
	}{
		{equal(3), 0.15},
		{equal(5), 0.15},
		{equal(10), 0.20},
		{[]int{3, 1}, 0.10},
		{[]int{4, 2, 1, 1}, 0.15},
	} {
		t.Run(fmt.Sprint(tt.weights), func(t *testing.T) {
			bs := hosts(tt.weights...)
			const n = 100000
			counts := make([]int, len(bs))
			for _, i := range assign(t, NewIPHash(bs), bs, clientIPs(n)) {
				counts[i]++
			}
			total := 0
			for _, w := range tt.weights {
				total += w
			}
			for i, got := range counts {
				want := float64(n) * float64(tt.weights[i]) / float64(total)
				if dev := (float64(got) - want) / want; dev < -tt.tolerance || dev > tt.tolerance {
					t.Errorf("backend %d got %d clients, want %.0f ± %.0f%%", i, got, want, 100*tt.tolerance)
				}
			}
		})
	}
}

// TestIPHashAddBackend checks the point of consistent hashing: adding a
// fifth backend moves about 1/5 of clients, and every client that moves
// goes to the new backend.
func TestIPHashAddBackend(t *testing.T) {
	for _, from := range []int{2, 4, 9} {
		t.Run(fmt.Sprintf("%d to %d", from, from+1), func(t *testing.T) {
			grown := hosts(equal(from + 1)...)
			before := grown[:from:from]
			ips := clientIPs(50000)
			was := assign(t, NewIPHash(before), before, ips)
			now := assign(t, NewIPHash(grown), grown, ips)
			moved := 0
			for i := range ips {
				if now[i] == was[i] {
					continue
				}
				moved++
				if now[i] != from {
					t.Fatalf("%v moved from backend %d to old backend %d", ips[i], was[i], now[i])
				}
			}
			want := 1 / float64(from+1)
			if got := float64(moved) / float64(len(ips)); got < want*0.75 || got > want*1.25 {
				t.Errorf("%.3f of clients moved, want about %.3f", got, want)
			}
		})
	}
}

// TestIPHashEject checks that when a backend goes down only its own clients
// move, they spread over the rest, and they come back when it recovers.
func TestIPHashEject(t *testing.T) {
	bs := hosts(equal(4)...)
	h := NewIPHash(bs)
	ips := clientIPs(20000)
	was := assign(t, h, bs, ips)

	bs[1].SetHealthy(false)
	now := assign(t, h, bs, ips)
	took := make([]int, len(bs))
	for i := range ips {
		switch {
		case was[i] != 1 && now[i] != was[i]:
			t.Fatalf("%v moved off healthy backend %d", ips[i], was[i])
		case was[i] == 1:
			took[now[i]]++
		}
	}
	if took[1] != 0 {
		t.Fatal("clients still sent to the backend that is down")
	}
	for i, n := range took {
		if i != 1 && n == 0 {
			t.Errorf("backend %d took none of the moved clients", i)
		}
	}

	bs[1].SetHealthy(true)
	back := assign(t, h, bs, ips)
	for i := range ips {
		if back[i] != was[i] {
			t.Fatalf("%v did not return to backend %d after recovery", ips[i], was[i])
		}
	}
}

func TestIPHashSkipsTried(t *testing.T) {
	bs := hosts(equal(3)...)
	h := NewIPHash(bs)
	for _, ip := range clientIPs(200) {
		first, _ := h.Pick(upstream.Choice{ClientIP: ip})
		retry, err := h.Pick(upstream.Choice{ClientIP: ip, Tried: []*upstream.Backend{first}})
		if err != nil || retry == first {
			t.Fatalf("%v: retry got %v, %v after trying %v", ip, retry, err, first)
		}
		again, _ := h.Pick(upstream.Choice{ClientIP: ip, Tried: []*upstream.Backend{first}})
		if again != retry {
			t.Fatalf("%v: retries are not sticky: %v then %v", ip, retry, again)
		}
	}
}

func TestIPHashNoIP(t *testing.T) {
	bs := hosts(equal(3)...)
	h := NewIPHash(bs)
	first, err := h.Pick(upstream.Choice{})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if b, _ := h.Pick(upstream.Choice{}); b != first {
			t.Fatal("clients without an IP do not share a backend")
		}
	}
}

func TestIPHashNoBackend(t *testing.T) {
	down := hosts(1, 1)
	for _, b := range down {
		b.SetHealthy(false)
	}
	all := hosts(1, 1, 1)
	ip := netip.MustParseAddr("192.0.2.1")
	for _, tt := range []struct {
		name string
		bs   []*upstream.Backend
		c    upstream.Choice
	}{
		{"empty", nil, upstream.Choice{ClientIP: ip}},
		{"all down", down, upstream.Choice{ClientIP: ip}},
		{"all tried", all, upstream.Choice{ClientIP: ip, Tried: all}},
	} {
		if _, err := NewIPHash(tt.bs).Pick(tt.c); !errors.Is(err, upstream.ErrNoBackend) {
			t.Errorf("%s: got %v, want ErrNoBackend", tt.name, err)
		}
	}
}

func TestIPHashRingSize(t *testing.T) {
	h := NewIPHash(hosts(3, 1)).(*IPHash)
	if got, want := len(h.ring), 4*pointsPerWeight; got != want {
		t.Errorf("ring has %d points, want %d", got, want)
	}
	// Huge weights are scaled down to the cap, keeping their ratio.
	bs := hosts(100000, 50000)
	h = NewIPHash(bs).(*IPHash)
	if len(h.ring) > maxPoints {
		t.Errorf("ring has %d points, want at most %d", len(h.ring), maxPoints)
	}
	var n [2]int
	for _, p := range h.ring {
		n[p.idx]++
	}
	if n[0] != 2*n[1] {
		t.Errorf("scaled points %v, want a 2:1 ratio", n)
	}
}

func TestIPHashNoAllocs(t *testing.T) {
	bs := hosts(equal(8)...)
	bs[3].SetHealthy(false)
	h := NewIPHash(bs)
	c := upstream.Choice{ClientIP: netip.MustParseAddr("2001:db8::1")}
	if n := testing.AllocsPerRun(100, func() { h.Pick(c) }); n != 0 {
		t.Errorf("Pick allocates %v times, want 0", n)
	}
}

func BenchmarkIPHash(b *testing.B) {
	h := NewIPHash(hosts(equal(8)...))
	ips := clientIPs(1024)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			h.Pick(upstream.Choice{ClientIP: ips[i%len(ips)]})
			i++
		}
	})
}
