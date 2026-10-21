package security

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// AuthConfig is what an Authenticator checks. Users map names to bcrypt
// hashes, and Keys map SHA-256 digests of API keys to the keys' names.
type AuthConfig struct {
	Realm string
	Users map[string]string // nil for no Basic auth
	// KeyHeader is where API keys are read from. When it is Authorization,
	// the key comes as "Bearer <key>".
	KeyHeader string
	Keys      map[[sha256.Size]byte]string // nil for no API keys
	// Forward keeps the credential headers on the request to the backend.
	Forward bool
	// IdentityHeader, when set, is removed from every request and then set
	// to the name of the user or key that was let in.
	IdentityHeader string
}

// Tuning for the cache of verified passwords.
const (
	verifiedTTL = 5 * time.Minute
	verifiedCap = 1024
)

// ErrUnauthorized is returned for missing or wrong credentials.
var ErrUnauthorized = errors.New("unauthorized")

// Authenticator checks the credentials of requests to one route. It is
// safe for concurrent use, and a reload with unchanged auth settings keeps
// it, along with its cache of verified passwords.
//
// bcrypt is slow on purpose, about 50 ms at cost 10, which would add that
// much to every request and let a few clients use up the CPU. Two things
// keep that in check: a password that passed is remembered for verifiedTTL,
// keyed by a SHA-256 of the user and password, and at most GOMAXPROCS
// bcrypt checks run at once, the rest waiting their turn.
type Authenticator struct {
	cfg       AuthConfig
	keyHeader string // canonical
	identity  string // canonical, or ""
	cost      int    // bcrypt cost of the dummy hash for unknown users
	cpu       chan struct{}
	now       func() time.Time

	mu       sync.Mutex
	verified map[[sha256.Size]byte]verifiedEntry
}

type verifiedEntry struct {
	hash    string // the hash it was checked against, which a reload may change
	expires time.Time
}

// NewAuthenticator returns an Authenticator for c, which must have users,
// keys or both. The hashes are assumed valid; config validation checks
// them.
func NewAuthenticator(c AuthConfig) *Authenticator {
	if c.Realm == "" {
		c.Realm = "prism"
	}
	a := &Authenticator{
		cfg:      c,
		cpu:      make(chan struct{}, runtime.GOMAXPROCS(0)),
		now:      time.Now,
		verified: map[[sha256.Size]byte]verifiedEntry{},
		cost:     bcrypt.MinCost,
	}
	if c.KeyHeader != "" {
		a.keyHeader = http.CanonicalHeaderKey(c.KeyHeader)
	}
	if c.IdentityHeader != "" {
		a.identity = http.CanonicalHeaderKey(c.IdentityHeader)
	}
	for _, h := range c.Users {
		if n, err := bcrypt.Cost([]byte(h)); err == nil && n > a.cost {
			a.cost = n
		}
	}
	return a
}

// Check returns the name of the user or key that r's credentials belong
// to. It fails with ErrUnauthorized for missing or wrong credentials, or
// with ctx's error if r's client leaves while waiting for a bcrypt slot.
func (a *Authenticator) Check(r *http.Request) (string, error) {
	if a.cfg.Keys != nil {
		if key, ok := a.apiKey(r.Header); ok {
			if name, ok := a.cfg.Keys[sha256.Sum256([]byte(key))]; ok {
				return name, nil
			}
			return "", ErrUnauthorized
		}
	}
	if a.cfg.Users != nil {
		if user, pass, ok := r.BasicAuth(); ok {
			return a.checkPassword(r.Context(), user, pass)
		}
	}
	return "", ErrUnauthorized
}

// apiKey returns the API key r carries, if it carries exactly one. Looking
// the key up by its digest leaks, through timing, at most something about
// the digest, which says nothing about any valid key.
func (a *Authenticator) apiKey(h http.Header) (string, bool) {
	vv := h[a.keyHeader]
	if len(vv) != 1 {
		return "", false
	}
	v := vv[0]
	if a.keyHeader == "Authorization" {
		scheme, token, ok := strings.Cut(v, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return "", false
		}
		v = strings.TrimSpace(token)
	}
	return v, v != ""
}

func (a *Authenticator) checkPassword(ctx context.Context, user, pass string) (string, error) {
	hash, known := a.cfg.Users[user]
	// The user is part of the key, so one user's password never lets in
	// another, and a separator keeps "ab"+"c" apart from "a"+"bc".
	key := sha256.Sum256([]byte(user + "\x00" + pass))
	if known && a.remembered(key, hash) {
		return user, nil
	}

	select {
	case a.cpu <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-a.cpu }()
	if !known {
		// Spend as long as a real check, so the time taken does not tell
		// which user names exist.
		bcrypt.CompareHashAndPassword(dummyHash(a.cost), []byte(pass))
		return "", ErrUnauthorized
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
		return "", ErrUnauthorized
	}
	a.remember(key, hash)
	return user, nil
}

func (a *Authenticator) remembered(key [sha256.Size]byte, hash string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.verified[key]
	if !ok {
		return false
	}
	if e.hash != hash || !a.now().Before(e.expires) {
		delete(a.verified, key)
		return false
	}
	return true
}

func (a *Authenticator) remember(key [sha256.Size]byte, hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.verified) >= verifiedCap {
		// Drop the expired entries, and if that frees nothing, an
		// arbitrary one. Only a client that knows many passwords can fill
		// the cache, and the cost of a miss is one more bcrypt check.
		now := a.now()
		for k, e := range a.verified {
			if !now.Before(e.expires) {
				delete(a.verified, k)
			}
		}
		for k := range a.verified {
			if len(a.verified) < verifiedCap {
				break
			}
			delete(a.verified, k)
		}
	}
	a.verified[key] = verifiedEntry{hash: hash, expires: a.now().Add(verifiedTTL)}
}

// Challenge returns the WWW-Authenticate value for a 401, or "" when the
// route takes only API keys in a header of its own, which has no standard
// challenge.
func (a *Authenticator) Challenge() string {
	switch {
	case a.cfg.Users != nil:
		return `Basic realm="` + a.cfg.Realm + `", charset="UTF-8"`
	case a.keyHeader == "Authorization":
		return `Bearer realm="` + a.cfg.Realm + `"`
	}
	return ""
}

// Prepare edits h, the header of the request going to the backend, for a
// request that user was let in as: it removes the credential headers,
// unless they are to be forwarded, and sets the identity header, replacing
// any the client sent to pose as someone else.
func (a *Authenticator) Prepare(h http.Header, user string) {
	if !a.cfg.Forward {
		if a.cfg.Users != nil {
			delete(h, "Authorization")
		}
		if a.cfg.Keys != nil {
			delete(h, a.keyHeader)
		}
	}
	if a.identity != "" {
		h[a.identity] = []string{user}
	}
}

// dummyHashes holds one bcrypt hash per cost, made the first time a request
// names an unknown user.
var dummyHashes sync.Map // int -> []byte

func dummyHash(cost int) []byte {
	if h, ok := dummyHashes.Load(cost); ok {
		return h.([]byte)
	}
	h, err := bcrypt.GenerateFromPassword([]byte("prism-dummy-password"), cost)
	if err != nil {
		// Only an out-of-range cost fails, which validation rules out.
		h, _ = bcrypt.GenerateFromPassword([]byte("prism-dummy-password"), bcrypt.DefaultCost)
	}
	actual, _ := dummyHashes.LoadOrStore(cost, h)
	return actual.([]byte)
}
