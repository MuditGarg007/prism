package observability

import (
	"io"
	"sync"
	"time"
)

// FlushInterval is how often a BufferedWriter flushes on its own, which
// bounds what a crash can lose.
const FlushInterval = time.Second

// BufferedWriter collects log lines in memory and writes them to w when the
// buffer fills, every interval and at Close. One write per line costs a
// syscall per request; a crash loses what is still buffered, which is why
// it is opt-in.
//
// A slog handler writes each record with one Write, and a record is never
// split across two writes to w, so lines stay whole even when another
// process shares w.
//
// It has two buffers. A full one is swapped for the spare under mu and
// written out after mu is released, so lines keep arriving while w is slow.
// Holding mu through the write instead stalled every request that logged
// meanwhile, and once a waiter has waited 1 ms Go's mutex hands it over in
// FIFO order, one wakeup at a time: at 20,000 requests per second that
// turned a slow write into stalls of 36 to 154 ms in bench runs. A writer now waits only
// when w falls a whole buffer behind.
type BufferedWriter struct {
	mu  sync.Mutex
	buf []byte // being filled
	err error  // the first write error, returned from then on

	// wmu is held while a buffer is written to w. It keeps the writes in
	// order and owns spare. Lock order: mu, then wmu.
	wmu   sync.Mutex
	w     io.Writer
	spare []byte

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewBufferedWriter returns a writer that buffers up to size bytes for w,
// in each of its two buffers, and flushes every interval. Close it to flush
// the rest and stop the timer.
func NewBufferedWriter(w io.Writer, size int, interval time.Duration) *BufferedWriter {
	b := &BufferedWriter{
		w:     w,
		buf:   make([]byte, 0, size),
		spare: make([]byte, 0, size),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go b.loop(interval)
	return b
}

func (b *BufferedWriter) loop(interval time.Duration) {
	defer close(b.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.Flush()
		case <-b.stop:
			return
		}
	}
}

// Write buffers p. If p does not fit, the full buffer is written out first,
// and an error from that write is returned by the next call. A p larger
// than a whole buffer goes straight to w, after what is buffered.
func (b *BufferedWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.err != nil {
		err := b.err
		b.mu.Unlock()
		return 0, err
	}
	if len(p) <= cap(b.buf)-len(b.buf) {
		b.buf = append(b.buf, p...)
		b.mu.Unlock()
		return len(p), nil
	}
	if len(p) > cap(b.buf) {
		// Rare enough to do in order with mu held.
		full := b.swapLocked()
		err := b.writeOut(full)
		n := 0
		if err == nil {
			n, err = b.w.Write(p)
		}
		b.wmu.Unlock()
		b.err = err
		b.mu.Unlock()
		return n, err
	}
	full := b.swapLocked()
	b.buf = append(b.buf, p...)
	b.mu.Unlock()
	err := b.writeOut(full)
	b.wmu.Unlock()
	if err != nil {
		b.keep(err)
	}
	return len(p), nil
}

// Flush writes what is buffered to w, after any write already under way.
func (b *BufferedWriter) Flush() error {
	b.mu.Lock()
	if b.err != nil {
		err := b.err
		b.mu.Unlock()
		return err
	}
	full := b.swapLocked()
	b.mu.Unlock()
	err := b.writeOut(full)
	b.wmu.Unlock()
	if err != nil {
		b.keep(err)
	}
	return err
}

// keep records err as the first write error, unless there already is one.
// It takes mu, so it must not be called with wmu held.
func (b *BufferedWriter) keep(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
}

// swapLocked, called with mu held, waits for any write under way, then
// swaps the spare in and returns the full buffer, with wmu held.
func (b *BufferedWriter) swapLocked() []byte {
	b.wmu.Lock()
	full := b.buf
	b.buf, b.spare = b.spare[:0], nil
	return full
}

// writeOut, called with wmu held, writes full to w and keeps it as the
// spare. The caller records any error, once wmu is released.
func (b *BufferedWriter) writeOut(full []byte) error {
	var err error
	if len(full) > 0 {
		_, err = b.w.Write(full)
	}
	b.spare = full[:0]
	return err
}

// Close stops the timer and flushes what is left. It does not close w.
func (b *BufferedWriter) Close() error {
	b.once.Do(func() { close(b.stop) })
	<-b.done
	return b.Flush()
}
