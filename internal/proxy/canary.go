package proxy

import (
	"math/rand/v2"
	"net/http"
	"net/netip"
	"strings"

	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/upstream"
)

// canaryBuckets is how finely Canary.Buckets splits clients: 10,000 buckets
// make 0.01% the smallest share.
const canaryBuckets = 10000

// Canary sends a share of a route's clients to another upstream.
type Canary struct {
	Pool *upstream.Pool
	// Buckets is how many of canaryBuckets go to Pool: percent × 100.
	Buckets uint32
	Header  string // canonical; "" for no header override
	Cookie  string // "" for no cookie override
	// Metrics are the route's handles labelled with Pool's name.
	Metrics *observability.RouteMetrics
}

// pick reports whether r, from client, goes to the canary.
//
// A header or cookie of "always" or "never" decides first, the header over
// the cookie. Otherwise the client's IP picks a bucket, so a client stays on
// one side while the share is unchanged, and raising the share only moves
// clients to the canary. Every route hashes the same way, so a client in
// the canary of one route is in that of every route with at least the same
// share. A client with no known IP gets a random bucket.
func (c *Canary) pick(r *http.Request, client netip.Addr) bool {
	if v, ok := c.override(r); ok {
		return v
	}
	var bucket uint32
	if client.IsValid() {
		bucket = canaryBucket(client)
	} else {
		bucket = rand.Uint32N(canaryBuckets)
	}
	return bucket < c.Buckets
}

func (c *Canary) override(r *http.Request) (canary, ok bool) {
	if c.Header != "" {
		if vv := r.Header[c.Header]; len(vv) > 0 {
			if v, ok := forced(vv[0]); ok {
				return v, true
			}
		}
	}
	if c.Cookie != "" {
		if ck, err := r.Cookie(c.Cookie); err == nil {
			return forced(ck.Value)
		}
	}
	return false, false
}

func forced(v string) (canary, ok bool) {
	switch {
	case strings.EqualFold(v, "always"):
		return true, true
	case strings.EqualFold(v, "never"):
		return false, true
	}
	return false, false
}

// canarySalt keeps the canary split independent of the ip_hash balancer,
// which hashes the same addresses: otherwise the canary's clients would all
// come from a few of the primary's backends.
const canarySalt = 0x6a09e667f3bcc908

// canaryBucket hashes a client address into [0, canaryBuckets) with
// FNV-1a and the splitmix64 finalizer. Nothing is seeded, so every Prism
// instance puts a client in the same bucket.
func canaryBucket(a netip.Addr) uint32 {
	b := a.Unmap().As16()
	h := uint64(14695981039346656037) ^ canarySalt
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return uint32(h % canaryBuckets)
}
