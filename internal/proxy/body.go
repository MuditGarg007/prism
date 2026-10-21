package proxy

import (
	"bytes"
	"io"
	"net/http"
	"sync/atomic"
)

// replayLimit is the largest request body read into memory so that a retry
// can send it again.
const replayLimit = 64 << 10

// requestBody hands each attempt at forwarding a request its body.
//
// On a route with retries, a body whose declared length is at most
// replayLimit is read up front, so every attempt gets the whole of it. Any
// other body is passed through once, and a retry is possible only while the
// transport has read none of it. A body passed through is counted, and its
// read errors are noted, so that a client's broken upload is never blamed on
// the backend.
//
// A body of unknown length is never read ahead, even a short one: a
// streaming client, such as a gRPC client stream, may wait for the response
// before it sends more, and reading ahead would deadlock it.
type requestBody struct {
	empty bool
	buf   []byte       // the whole body, when read up front
	read  *readCounter // the client's body otherwise
}

func newRequestBody(r *http.Request, retry bool) (requestBody, error) {
	switch {
	case r.ContentLength == 0:
		return requestBody{empty: true}, nil
	case retry && r.ContentLength > 0 && r.ContentLength <= replayLimit:
		buf := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			return requestBody{}, err
		}
		return requestBody{buf: buf}, nil
	default:
		return requestBody{read: &readCounter{r: r.Body}}, nil
	}
}

// attempt returns the body for the next attempt, and a GetBody that lets the
// transport rewind it when it retries a stale pooled connection by itself.
func (b *requestBody) attempt() (io.ReadCloser, func() (io.ReadCloser, error)) {
	switch {
	case b.empty:
		return http.NoBody, nil
	case b.buf != nil:
		buf := b.buf // not b, which would then escape on every request
		get := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		body, _ := get()
		return body, get
	default:
		return b.read, nil
	}
}

// replayable reports whether the body can be sent again in full.
func (b *requestBody) replayable() bool {
	return b.read == nil || b.read.n.Load() == 0
}

// clientFailed reports whether reading the client's body failed, including
// by passing max_body, which makes an attempt's failure the client's fault.
func (b *requestBody) clientFailed() bool {
	return b.read != nil && b.read.failed.Load()
}

// readCounter counts the bytes read from a client's body. Close does not
// close the client's body: the transport closes a request's body when an
// attempt fails, and a retry still needs it. net/http closes it once the
// handler returns.
type readCounter struct {
	r      io.Reader
	n      atomic.Int64 // the transport reads on its own goroutine
	failed atomic.Bool  // a read returned an error other than io.EOF

	shadow   *shadowCapture         // nil unless the body is copied for a shadow
	captured atomic.Pointer[[]byte] // the whole body, once shadow has it
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.shadow != nil && n > 0 {
		c.keep(p[:n])
	}
	c.n.Add(int64(n))
	if err != nil && err != io.EOF {
		c.failed.Store(true)
	}
	return n, err
}

func (c *readCounter) Close() error { return nil }
