package cache

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
)

func TestPrimaryKey(t *testing.T) {
	tests := []struct {
		method, target string
		tls            bool
		want           string
	}{
		{"GET", "http://Example.COM/a?x=1&y=2", false, "GET http://example.com/a?x=1&y=2"},
		{"HEAD", "http://example.com/a", false, "GET http://example.com/a"},
		{"GET", "http://example.com:80/a", false, "GET http://example.com/a"},
		{"GET", "http://example.com:8080/a", false, "GET http://example.com:8080/a"},
		{"GET", "https://example.com:443/a", true, "GET https://example.com/a"},
		{"GET", "https://example.com:80/a", true, "GET https://example.com:80/a"},
		{"GET", "http://example.com/a%2Fb", false, "GET http://example.com/a%2Fb"},
		{"GET", "http://example.com/a/b", false, "GET http://example.com/a/b"},
		{"GET", "http://example.com/a?", false, "GET http://example.com/a"},
		{"GET", "http://example.com/a?b=2&a=1", false, "GET http://example.com/a?b=2&a=1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, tt.target, nil)
		if tt.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := PrimaryKey(r); got != tt.want {
			t.Errorf("PrimaryKey(%s %s) = %q, want %q", tt.method, tt.target, got, tt.want)
		}
	}
}

func TestVaryNames(t *testing.T) {
	h := http.Header{"Vary": {"accept-encoding, Accept", " ACCEPT-ENCODING ,, x-tenant"}}
	if got, want := VaryNames(h), []string{"Accept", "Accept-Encoding", "X-Tenant"}; !slices.Equal(got, want) {
		t.Errorf("VaryNames = %q, want %q", got, want)
	}
	if got := VaryNames(http.Header{}); got != nil {
		t.Errorf("VaryNames without Vary = %q, want nil", got)
	}
}

func TestVariantKey(t *testing.T) {
	names := []string{"Accept-Encoding", "X-Tenant"}
	key := func(h http.Header) string { return VariantKey("GET http://a/", names, h) }
	same := []http.Header{
		{"Accept-Encoding": {"gzip, br"}},
		{"Accept-Encoding": {"gzip,br"}},
		{"Accept-Encoding": {" gzip ", " br"}},
		{"Accept-Encoding": {"gzip,, br"}, "X-Tenant": {""}},
	}
	for _, h := range same[1:] {
		if key(h) != key(same[0]) {
			t.Errorf("%v gives %q, want the same key as %v: %q", h, key(h), same[0], key(same[0]))
		}
	}
	different := []http.Header{
		{"Accept-Encoding": {"br, gzip"}},
		{"Accept-Encoding": {"gzip, br"}, "X-Tenant": {"a"}},
		{},
	}
	for _, h := range different {
		if key(h) == key(same[0]) {
			t.Errorf("%v gives the same key as %v", h, same[0])
		}
	}
	if got := VariantKey("GET http://a/", nil, http.Header{"Accept": {"x"}}); got != "GET http://a/" {
		t.Errorf("key without Vary = %q, want the primary key", got)
	}
}

// response builds an entry for primary with the given Vary, as stored for a
// request with header req.
func response(primary, vary string, req http.Header, body string) *Entry {
	h := http.Header{}
	if vary != "" {
		h.Set("Vary", vary)
	}
	return NewEntry(primary, req, 200, h, []byte(body), Directives{}, Freshness{})
}

func TestStoreVariants(t *testing.T) {
	s := NewStore(1 << 20)
	const p = "GET http://a/x"
	gz := http.Header{"Accept-Encoding": {"gzip"}}
	br := http.Header{"Accept-Encoding": {"br"}}
	s.Put(response(p, "Accept-Encoding", gz, "gz"))
	s.Put(response(p, "Accept-Encoding", br, "br"))

	for _, tt := range []struct {
		req  http.Header
		want string
	}{{gz, "gz"}, {br, "br"}, {http.Header{"Accept-Encoding": {"zstd"}}, ""}, {http.Header{}, ""}} {
		e, ok := s.Get(p, tt.req)
		if got := ""; ok {
			got = string(e.Body)
			if got != tt.want {
				t.Errorf("Get(%v) = %q, want %q", tt.req, got, tt.want)
			}
		} else if tt.want != "" {
			t.Errorf("Get(%v) missed, want %q", tt.req, tt.want)
		}
	}
	if _, ok := s.Get("GET http://a/other", gz); ok {
		t.Error("hit on a primary key never stored")
	}
	if st := s.Stats(); st.Entries != 2 || s.vary[p].n != 2 {
		t.Errorf("entries = %d, index count = %d; want 2, 2", st.Entries, s.vary[p].n)
	}
}

func TestStoreNoVary(t *testing.T) {
	s := NewStore(1 << 20)
	s.Put(response("GET http://a/x", "", http.Header{"Accept-Encoding": {"gzip"}}, "body"))
	if e, ok := s.Get("GET http://a/x", http.Header{"Accept-Encoding": {"br"}}); !ok || string(e.Body) != "body" {
		t.Error("a response without Vary must serve every request")
	}
}

// TestStoreVaryChanges checks that when a backend changes its Vary, lookups
// use the new names, and the old variants can no longer match.
func TestStoreVaryChanges(t *testing.T) {
	s := NewStore(1 << 20)
	const p = "GET http://a/x"
	req := http.Header{"Accept-Encoding": {"gzip"}, "Accept": {"text/html"}}
	s.Put(response(p, "Accept-Encoding", req, "old"))
	s.Put(response(p, "Accept", req, "new"))
	if e, ok := s.Get(p, req); !ok || string(e.Body) != "new" {
		t.Errorf("Get = %v, %v; want the variant stored under the new Vary", e, ok)
	}
	if n := s.vary[p].n; n != 2 {
		t.Errorf("index count = %d, want 2 (both variants are still stored)", n)
	}
}

func TestStoreIndexFollowsEviction(t *testing.T) {
	e1 := response("GET http://a/1", "Accept", http.Header{}, "")
	e2 := response("GET http://a/2", "", http.Header{}, "")
	s := NewStore(e1.Size() + e2.Size())
	s.Put(e1)
	s.Put(e2)
	s.Put(response("GET http://a/3", "", http.Header{}, "")) // evicts e1
	if _, ok := s.vary["GET http://a/1"]; ok {
		t.Error("index entry kept after its last variant was evicted")
	}
	if st := s.Stats(); st.Entries != 2 || st.Evictions != 1 {
		t.Errorf("stats = %+v, want 2 entries, 1 eviction", st)
	}
}

func TestStoreResize(t *testing.T) {
	var es []*Entry
	for i := range 4 {
		es = append(es, response(fmt.Sprintf("GET http://a/%d", i), "", http.Header{}, "body"))
	}
	size := es[0].Size()
	s := NewStore(4 * size)
	for _, e := range es {
		s.Put(e)
	}
	s.Get(es[0].Primary, http.Header{}) // 1 is now the least recently used

	s.Resize(2*size + size/2)
	if st := s.Stats(); st.Entries != 2 || st.Bytes != 2*size || st.Evictions != 2 {
		t.Errorf("after shrinking: %+v, want 2 entries, 2 evictions", st)
	}
	for i, want := range []bool{true, false, false, true} {
		if _, ok := s.Get(es[i].Primary, http.Header{}); ok != want {
			t.Errorf("entry %d kept = %v, want %v", i, ok, want)
		}
	}
	if _, ok := s.vary[es[1].Primary]; ok {
		t.Error("the Vary index kept an evicted entry")
	}

	s.Resize(4 * size)
	s.Put(es[1])
	s.Put(es[2])
	if st := s.Stats(); st.Entries != 4 || st.Evictions != 2 {
		t.Errorf("after growing: %+v, want all 4 entries and no new evictions", st)
	}
}

func TestStoreRemoveOnlyThatEntry(t *testing.T) {
	s := NewStore(1 << 20)
	old := response("GET http://a/x", "", http.Header{}, "old")
	s.Put(old)
	s.Put(response("GET http://a/x", "", http.Header{}, "new"))
	if s.Remove(old) {
		t.Error("Remove deleted the newer entry stored under the same key")
	}
	e, _ := s.Get("GET http://a/x", http.Header{})
	if !s.Remove(e) || s.Stats().Entries != 0 || len(s.vary) != 0 {
		t.Errorf("Remove of the current entry left %+v and index %v", s.Stats(), s.vary)
	}
}

func TestStorePurge(t *testing.T) {
	s := NewStore(1 << 20)
	put := func(primary string) {
		s.Put(response(primary, "Accept", http.Header{"Accept": {"a"}}, ""))
		s.Put(response(primary, "Accept", http.Header{"Accept": {"b"}}, ""))
	}
	for _, p := range []string{
		"GET http://a.com/x", "GET https://a.com/x", "GET http://a.com/x?q=1",
		"GET http://a.com/xyz", "GET http://a.com/y", "GET http://a.company/x",
	} {
		put(p)
	}
	if n := s.Purge("A.com", "/x", false); n != 4 {
		t.Errorf("exact purge removed %d, want 4 (2 schemes x 2 variants)", n)
	}
	if n := s.Purge("a.com", "/x", true); n != 4 {
		t.Errorf("prefix purge removed %d, want 4 (/x?q=1 and /xyz)", n)
	}
	if st := s.Stats(); st.Entries != 4 {
		t.Errorf("%d entries left, want 4 (a.com/y and a.company/x)", st.Entries)
	}
	if len(s.vary) != 2 {
		t.Errorf("index has %d primary keys, want 2", len(s.vary))
	}
}

func TestStoreConcurrent(t *testing.T) {
	s := NewStore(64 * (entryOverhead + 200))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				p := "GET http://a/" + strconv.Itoa((g*7+i)%100)
				req := http.Header{"Accept": {strconv.Itoa(i % 3)}}
				switch i % 4 {
				case 0:
					s.Put(response(p, "Accept", req, "x"))
				case 3:
					if e, ok := s.Get(p, req); ok {
						s.Remove(e)
					}
				default:
					s.Get(p, req)
				}
			}
		})
	}
	wg.Wait()
	st := s.Stats()
	total := 0
	for _, idx := range s.vary {
		total += idx.n
	}
	if total != st.Entries {
		t.Errorf("index counts %d variants, store holds %d", total, st.Entries)
	}
}

func BenchmarkStoreGet(b *testing.B) {
	s := NewStore(1 << 30)
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprintf("GET http://example.com/item/%d", i)
		s.Put(response(keys[i], "", nil, "body"))
	}
	req := http.Header{}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for i := 0; pb.Next(); i++ {
			s.Get(keys[i&4095], req)
		}
	})
}

func BenchmarkStoreGetVary(b *testing.B) {
	s := NewStore(1 << 30)
	req := http.Header{"Accept-Encoding": {"gzip, br"}}
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprintf("GET http://example.com/item/%d", i)
		s.Put(response(keys[i], "Accept-Encoding", req, "body"))
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for i := 0; pb.Next(); i++ {
			s.Get(keys[i&4095], req)
		}
	})
}
