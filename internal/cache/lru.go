package cache

// lru holds entries up to a total size in bytes, evicting the least recently
// used ones to make room. It is a map for lookups plus a doubly linked list
// kept in order of use, most recent at the front, so every operation is O(1)
// except removeFunc.
//
// An lru is not safe for concurrent use; Store locks around it.
type lru struct {
	maxBytes int64
	bytes    int64
	items    map[string]*node
	root     node // sentinel: root.next is the most recent, root.prev the least

	// onRemove, if set, is called for every entry that leaves, whether
	// evicted, replaced or removed.
	onRemove  func(e *Entry)
	evictions uint64
}

type node struct {
	prev, next *node
	entry      *Entry
	size       int64
}

func newLRU(maxBytes int64) *lru {
	l := &lru{maxBytes: maxBytes, items: make(map[string]*node)}
	l.root.next, l.root.prev = &l.root, &l.root
	return l
}

// get returns the entry under key and marks it as the most recently used.
func (l *lru) get(key string) (*Entry, bool) {
	n, ok := l.items[key]
	if !ok {
		return nil, false
	}
	l.moveToFront(n)
	return n.entry, true
}

// add stores e under e.Key, replacing any entry already there, and evicts
// the least recently used entries until everything fits. An entry larger
// than the whole cache is not stored, and add returns false.
func (l *lru) add(e *Entry) bool {
	size := e.Size()
	if size > l.maxBytes {
		return false
	}
	if old, ok := l.items[e.Key]; ok {
		l.unlink(old)
	}
	for l.bytes+size > l.maxBytes {
		l.unlink(l.root.prev)
		l.evictions++
	}
	n := &node{entry: e, size: size}
	l.items[e.Key] = n
	l.bytes += size
	l.pushFront(n)
	return true
}

// remove deletes the entry under key and reports whether there was one.
func (l *lru) remove(key string) bool {
	n, ok := l.items[key]
	if ok {
		l.unlink(n)
	}
	return ok
}

// removeFunc deletes every entry for which match returns true and returns
// how many it deleted. It walks the whole cache.
func (l *lru) removeFunc(match func(e *Entry) bool) int {
	removed := 0
	for n := l.root.next; n != &l.root; {
		next := n.next
		if match(n.entry) {
			l.unlink(n)
			removed++
		}
		n = next
	}
	return removed
}

func (l *lru) len() int { return len(l.items) }

// unlink takes n out of the list and the map and calls onRemove.
func (l *lru) unlink(n *node) {
	n.prev.next, n.next.prev = n.next, n.prev
	n.prev, n.next = nil, nil
	delete(l.items, n.entry.Key)
	l.bytes -= n.size
	if l.onRemove != nil {
		l.onRemove(n.entry)
	}
}

func (l *lru) pushFront(n *node) {
	n.prev, n.next = &l.root, l.root.next
	l.root.next.prev = n
	l.root.next = n
}

func (l *lru) moveToFront(n *node) {
	if l.root.next == n {
		return
	}
	n.prev.next, n.next.prev = n.next, n.prev
	l.pushFront(n)
}
