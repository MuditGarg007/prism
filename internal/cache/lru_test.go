package cache

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
)

// sized returns an entry under key whose Size is exactly size, which must be
// at least entryOverhead+len(key).
func sized(key string, size int) *Entry {
	return &Entry{Key: key, Body: make([]byte, size-entryOverhead-len(key))}
}

const unit = entryOverhead + 100 // size of the entries most tests use

func keys(l *lru) []string {
	var out []string
	for n := l.root.next; n != &l.root; n = n.next {
		out = append(out, n.entry.Key)
	}
	return out
}

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	l := newLRU(3 * unit)
	var removed []string
	l.onRemove = func(e *Entry) { removed = append(removed, e.Key) }
	for _, k := range []string{"a", "b", "c"} {
		if !l.add(sized(k, unit)) {
			t.Fatalf("add %s failed", k)
		}
	}
	if _, ok := l.get("a"); !ok { // a is now the most recent
		t.Fatal("a missing")
	}
	l.add(sized("d", unit))
	if got, want := keys(l), []string{"d", "a", "c"}; !slices.Equal(got, want) {
		t.Errorf("order = %q, want %q", got, want)
	}
	if !slices.Equal(removed, []string{"b"}) || l.evictions != 1 {
		t.Errorf("removed %q with %d evictions, want [b] and 1", removed, l.evictions)
	}
	if _, ok := l.get("b"); ok {
		t.Error("b still there after eviction")
	}
	if l.bytes != 3*unit || l.len() != 3 {
		t.Errorf("bytes = %d, len = %d; want %d, 3", l.bytes, l.len(), 3*unit)
	}
}

func TestLRUBigEntryEvictsSeveral(t *testing.T) {
	l := newLRU(4 * unit)
	for _, k := range []string{"a", "b", "c", "d"} {
		l.add(sized(k, unit))
	}
	l.add(sized("big", 3*unit))
	if got, want := keys(l), []string{"big", "d"}; !slices.Equal(got, want) {
		t.Errorf("keys = %q, want %q", got, want)
	}
	if l.evictions != 3 {
		t.Errorf("evictions = %d, want 3", l.evictions)
	}
}

func TestLRUReplace(t *testing.T) {
	l := newLRU(10 * unit)
	var removed int
	l.onRemove = func(*Entry) { removed++ }
	l.add(sized("a", unit))
	l.add(sized("b", unit))
	l.add(sized("a", 2*unit))
	if l.bytes != 3*unit || l.len() != 2 {
		t.Errorf("bytes = %d, len = %d; want %d, 2", l.bytes, l.len(), 3*unit)
	}
	if got := keys(l); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("keys = %q, want a first", got)
	}
	if removed != 1 || l.evictions != 0 {
		t.Errorf("replace: %d removals, %d evictions; want 1, 0", removed, l.evictions)
	}
}

func TestLRUTooBig(t *testing.T) {
	l := newLRU(2 * unit)
	l.add(sized("a", unit))
	if l.add(sized("huge", 2*unit+1)) {
		t.Error("stored an entry larger than the cache")
	}
	if _, ok := l.get("a"); !ok || l.evictions != 0 {
		t.Error("a too-big entry evicted others")
	}
}

func TestLRURemove(t *testing.T) {
	l := newLRU(10 * unit)
	for _, k := range []string{"a/1", "b/1", "a/2", "c"} {
		l.add(sized(k, unit))
	}
	if !l.remove("c") || l.remove("c") {
		t.Error("remove c: want true, then false")
	}
	n := l.removeFunc(func(e *Entry) bool { return e.Key[0] == 'a' })
	if n != 2 || !slices.Equal(keys(l), []string{"b/1"}) || l.bytes != unit {
		t.Errorf("removeFunc removed %d, left %q (%d bytes); want 2, [b/1], %d", n, keys(l), l.bytes, unit)
	}
}

// TestLRUModel runs random operations against both the lru and a plain
// slice ordered by use, and checks they agree after each one.
func TestLRUModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	const maxBytes = 20 * unit
	l := newLRU(maxBytes)
	type item struct {
		key  string
		size int64
	}
	var model []item // most recent first
	find := func(k string) int {
		return slices.IndexFunc(model, func(it item) bool { return it.key == k })
	}
	for range 20000 {
		k := strconv.Itoa(rng.IntN(40))
		switch rng.IntN(3) {
		case 0:
			_, ok := l.get(k)
			if i := find(k); (i >= 0) != ok {
				t.Fatalf("get %s: lru %v, model %v", k, ok, i >= 0)
			} else if ok {
				it := model[i]
				model = slices.Insert(slices.Delete(model, i, i+1), 0, it)
			}
		case 1:
			size := entryOverhead + len(k) + rng.IntN(4*unit)
			l.add(sized(k, size))
			if i := find(k); i >= 0 {
				model = slices.Delete(model, i, i+1)
			}
			var total int64
			for _, it := range model {
				total += it.size
			}
			for total+int64(size) > maxBytes {
				total -= model[len(model)-1].size
				model = model[:len(model)-1]
			}
			model = slices.Insert(model, 0, item{k, int64(size)})
		case 2:
			ok := l.remove(k)
			i := find(k)
			if (i >= 0) != ok {
				t.Fatalf("remove %s: lru %v, model %v", k, ok, i >= 0)
			}
			if ok {
				model = slices.Delete(model, i, i+1)
			}
		}
		var want []string
		var bytes int64
		for _, it := range model {
			want = append(want, it.key)
			bytes += it.size
		}
		if got := keys(l); !slices.Equal(got, want) || l.bytes != bytes || l.len() != len(want) {
			t.Fatalf("lru %q (%d bytes, len %d), model %q (%d bytes)", got, l.bytes, l.len(), want, bytes)
		}
	}
}

func BenchmarkLRUGet(b *testing.B) {
	l := newLRU(1 << 30)
	ks := make([]string, 4096)
	for i := range ks {
		ks[i] = "GET http://example.com/item/" + strconv.Itoa(i)
		l.add(&Entry{Key: ks[i]})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		l.get(ks[i&4095])
	}
}

func BenchmarkLRUAddEvict(b *testing.B) {
	l := newLRU(1024 * entryOverhead * 2)
	ks := make([]string, 8192)
	for i := range ks {
		ks[i] = "GET http://example.com/item/" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		l.add(&Entry{Key: ks[i&8191]})
	}
}
