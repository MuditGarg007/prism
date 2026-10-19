package balancer

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/MuditGarg007/prism/internal/upstream"
)

const (
	// pointsPerWeight is how many points each unit of weight puts on the
	// ring, as in Ketama. More points even out the share of each backend.
	pointsPerWeight = 160
	// maxPoints caps the ring so large weights cannot make it huge; weights
	// are scaled down to fit.
	maxPoints = 1 << 16
)

// IPHash sends each client IP to the same backend with consistent hashing.
// Every backend owns pointsPerWeight×weight points on a hash ring, and a
// client goes to the owner of the first point at or after its own hash.
// Adding a backend therefore remaps only the clients whose next point is now
// one of the new backend's, about 1/N of them, instead of nearly all of them
// as hash mod N would.
//
// When a client's backend cannot be used, the walk continues clockwise to
// the next usable one. Its points are spread around the ring, so its clients
// spread over the other backends, and nobody else moves.
//
// Hashes are fixed functions of the backend URL and the client IP, not
// seeded per process, so every Prism instance with the same backends sends a
// client to the same place.
type IPHash struct {
	bs   []*upstream.Backend
	ring []point // sorted by hash
}

type point struct {
	hash uint64
	idx  int32 // index into bs
}

// NewIPHash returns a consistent-hashing balancer over bs, keyed on the
// client IP.
func NewIPHash(bs []*upstream.Backend) upstream.Balancer {
	total := 0
	for _, b := range bs {
		total += pointsPerWeight * weight(b)
	}
	scale := 1.0
	if total > maxPoints {
		scale = float64(maxPoints) / float64(total)
	}
	h := &IPHash{bs: bs, ring: make([]point, 0, min(total, maxPoints+len(bs)))}
	for i, b := range bs {
		n := max(1, int(float64(pointsPerWeight*weight(b))*scale))
		key := b.URL.String() + "-"
		for v := range n {
			h.ring = append(h.ring, point{hash: hashBytes([]byte(key + strconv.Itoa(v))), idx: int32(i)})
		}
	}
	// Equal hashes are vanishingly rare; ordering them by index keeps the
	// ring the same on every instance anyway.
	slices.SortFunc(h.ring, func(a, b point) int {
		return cmp.Or(cmp.Compare(a.hash, b.hash), cmp.Compare(a.idx, b.idx))
	})
	return h
}

func (h *IPHash) Pick(c upstream.Choice) (*upstream.Backend, error) {
	n := len(h.ring)
	if n == 0 {
		return nil, upstream.ErrNoBackend
	}
	// A zero Addr (a non-IP peer) hashes as all zeros: all such clients
	// share one backend, which is the best that can be done without an IP.
	ip := c.ClientIP.As16()
	key := hashBytes(ip[:])
	start, _ := slices.BinarySearchFunc(h.ring, key, func(p point, k uint64) int {
		return cmp.Compare(p.hash, k)
	})
	// One lap at most. Consecutive points often belong to the same backend,
	// so remember the last one checked.
	last := int32(-1)
	for i := range n {
		p := h.ring[(start+i)%n]
		if p.idx == last {
			continue
		}
		last = p.idx
		if b := h.bs[p.idx]; c.Usable(b) {
			return b, nil
		}
	}
	return nil, upstream.ErrNoBackend
}

// hashBytes is 64-bit FNV-1a followed by the splitmix64 finalizer. FNV alone
// spreads short, similar keys such as "url-1", "url-2" poorly in its high
// bits; the finalizer mixes every input bit into every output bit.
func hashBytes(data []byte) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	z := uint64(offset)
	for _, c := range data {
		z ^= uint64(c)
		z *= prime
	}
	z ^= z >> 30
	z *= 0xbf58476d1ce4e5b9
	z ^= z >> 27
	z *= 0x94d049bb133111eb
	z ^= z >> 31
	return z
}
