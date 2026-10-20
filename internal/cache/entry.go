package cache

import "net/http"

// entryOverhead is a rough count of the bytes an entry costs beyond its key,
// header and body: the list node, the Entry, the map slot and the header
// map's buckets. It keeps a cache full of tiny responses from holding far
// more memory than max_size says.
const entryOverhead = 512

// Entry is a stored response. It is never changed once stored; a
// revalidation that updates it stores a new Entry that shares the body.
type Entry struct {
	Key       string // the full key, primary key plus Vary values
	Primary   string // the key without the Vary values
	Vary      []string
	Status    int
	Header    http.Header
	Body      []byte
	CC        Directives // the response's Cache-Control
	Freshness Freshness
}

// Size returns roughly how many bytes e holds.
func (e *Entry) Size() int64 {
	n := entryOverhead + len(e.Key) + len(e.Primary) + len(e.Body)
	for _, name := range e.Vary {
		n += len(name)
	}
	for k, vv := range e.Header {
		n += len(k)
		for _, v := range vv {
			n += len(v) + 16 // the string header in the slice
		}
	}
	return int64(n)
}
