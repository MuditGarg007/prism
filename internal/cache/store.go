package cache

import (
	"net/http"
	"strings"
	"sync"
)

// Store is the response cache that every route shares, bounded in bytes. It
// is safe for concurrent use. Entries are immutable, so a caller may keep
// writing one to a client after the Store has dropped it.
//
// Responses that vary are stored once per variant. An index from each
// primary key to the Vary names of its latest response says which request
// headers pick the variant, so a lookup is one index read and one LRU read
// under a single lock.
type Store struct {
	mu   sync.Mutex
	lru  *lru
	vary map[string]*varyIndex
}

type varyIndex struct {
	names []string // from the most recently stored variant
	n     int      // variants stored under the primary key
}

// StoreStats is a snapshot of a Store's size.
type StoreStats struct {
	Entries   int
	Bytes     int64
	Evictions uint64
}

// NewStore returns an empty Store that holds up to maxBytes.
func NewStore(maxBytes int64) *Store {
	s := &Store{lru: newLRU(maxBytes), vary: make(map[string]*varyIndex)}
	s.lru.onRemove = s.forget
	return s
}

// NewEntry builds an entry for a response with status code, header h and
// body to a request for primary with header req. The entry's Vary and Key
// come from h's Vary and req.
func NewEntry(primary string, req http.Header, code int, h http.Header, body []byte, cc Directives, f Freshness) *Entry {
	names := VaryNames(h)
	return &Entry{
		Key:       VariantKey(primary, names, req),
		Primary:   primary,
		Vary:      names,
		Status:    code,
		Header:    h,
		Body:      body,
		CC:        cc,
		Freshness: f,
	}
}

// Get returns the entry stored for primary that matches the Vary headers
// of a request with header req, and marks it recently used. It returns
// entries whether or not they are still fresh; the caller decides.
func (s *Store) Get(primary string, req http.Header) (*Entry, bool) {
	_, e, ok := s.Lookup(primary, req)
	return e, ok
}

// Lookup is Get that also returns the key the request maps to, which is
// the entry's key when one is found. With nothing stored for primary, the
// key is primary itself. Requests that would share an entry share the key,
// so it is the one to coalesce fetches on.
func (s *Store) Lookup(primary string, req http.Header) (string, *Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.vary[primary]
	if !ok {
		return primary, nil, false
	}
	key := VariantKey(primary, idx.names, req)
	e, ok := s.lru.get(key)
	return key, e, ok
}

// Put stores e, replacing any entry under its key, and reports whether it
// fit. Least recently used entries are evicted to make room.
func (s *Store) Put(e *Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lru.add(e) {
		return false
	}
	idx, ok := s.vary[e.Primary]
	if !ok {
		idx = &varyIndex{}
		s.vary[e.Primary] = idx
	}
	idx.names = e.Vary
	idx.n++
	return true
}

// Remove deletes e if it is still the entry under its key, so a caller that
// read e cannot delete a newer entry stored since. It reports whether it
// deleted anything.
func (s *Store) Remove(e *Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.lru.items[e.Key]; !ok || cur.entry != e {
		return false
	}
	return s.lru.remove(e.Key)
}

// Purge deletes every variant of every method and scheme for host and
// path, where path includes any query. With prefix set it deletes every
// path that starts with path instead, queries included. host is matched
// without case and must carry its port if not the default. It returns how
// many entries it deleted, walking the whole store.
func (s *Store) Purge(host, path string, prefix bool) int {
	target := strings.ToLower(host) + path
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lru.removeFunc(func(e *Entry) bool {
		u := urlPart(e.Primary)
		if prefix {
			return strings.HasPrefix(u, target)
		}
		return u == target
	})
}

// Resize changes the Store's byte budget, evicting the least recently used
// entries until what is held fits. A reload calls it when max_size changes,
// so the cache stays warm.
func (s *Store) Resize(maxBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lru
	l.maxBytes = maxBytes
	for l.bytes > l.maxBytes {
		l.unlink(l.root.prev)
		l.evictions++
	}
}

// Stats returns the Store's current size.
func (s *Store) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StoreStats{Entries: s.lru.len(), Bytes: s.lru.bytes, Evictions: s.lru.evictions}
}

// forget drops e's count from the Vary index, and the index entry with its
// last variant. The lru calls it with s.mu held.
func (s *Store) forget(e *Entry) {
	idx, ok := s.vary[e.Primary]
	if !ok {
		return
	}
	if idx.n--; idx.n <= 0 {
		delete(s.vary, e.Primary)
	}
}
