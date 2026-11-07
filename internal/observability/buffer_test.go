package observability

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder keeps every Write it gets, so tests can see how lines were
// grouped.
type recorder struct {
	mu     sync.Mutex
	writes []string
	err    error
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	r.writes = append(r.writes, string(p))
	return len(p), nil
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.writes...)
}

func TestBufferedWriter(t *testing.T) {
	tests := []struct {
		name   string
		size   int
		lines  []string
		writes []string // what reaches w by Close, one entry per Write
	}{
		{"held until close", 64, []string{"a\n", "b\n"}, []string{"a\nb\n"}},
		{"flushes before a line that does not fit", 6, []string{"aa\n", "bb\n", "cc\n"}, []string{"aa\nbb\n", "cc\n"}},
		{"line exactly the size", 3, []string{"aa\n", "bb\n"}, []string{"aa\n", "bb\n"}},
		{"oversized line goes straight through", 4, []string{"a\n", "bbbbbb\n", "c\n"}, []string{"a\n", "bbbbbb\n", "c\n"}},
		{"nothing written", 16, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r recorder
			b := NewBufferedWriter(&r, tt.size, time.Hour)
			for _, l := range tt.lines {
				if n, err := b.Write([]byte(l)); n != len(l) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", l, n, err)
				}
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if got := r.got(); strings.Join(got, "|") != strings.Join(tt.writes, "|") {
				t.Errorf("writes = %q, want %q", got, tt.writes)
			}
		})
	}
}

func TestBufferedWriterFlushesOnInterval(t *testing.T) {
	var r recorder
	b := NewBufferedWriter(&r, 1<<10, 10*time.Millisecond)
	defer b.Close()
	b.Write([]byte("line\n"))
	deadline := time.Now().Add(5 * time.Second)
	for len(r.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the line was never flushed")
		}
		time.Sleep(time.Millisecond)
	}
	if got := r.got(); len(got) != 1 || got[0] != "line\n" {
		t.Errorf("writes = %q", got)
	}
}

func TestBufferedWriterKeepsError(t *testing.T) {
	r := recorder{err: errors.New("disk full")}
	b := NewBufferedWriter(&r, 4, time.Hour)
	defer b.Close()
	b.Write([]byte("abc\n"))
	if err := b.Flush(); err == nil {
		t.Fatal("Flush succeeded, want the write error")
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Error("Write after a failed flush succeeded, want the error again")
	}
}

// gated is a writer whose writes wait until open is closed.
type gated struct {
	recorder
	open chan struct{}
}

func (g *gated) Write(p []byte) (int, error) {
	<-g.open
	return g.recorder.Write(p)
}

// TestBufferedWriterSlowWrite checks that a line that fits is buffered at
// once while a full buffer is still being written to a slow w, and that the
// lines still reach w in order.
func TestBufferedWriterSlowWrite(t *testing.T) {
	g := &gated{open: make(chan struct{})}
	b := NewBufferedWriter(g, 8, time.Hour)
	b.Write([]byte("aaaa\n"))
	flushing := make(chan struct{})
	go func() {
		defer close(flushing)
		b.Write([]byte("bbbb\n")) // does not fit: writes "aaaa\n" out, and waits on g
	}()
	wrote := make(chan struct{})
	go func() {
		// Wait for the swap, so this line lands behind "bbbb\n".
		for {
			b.mu.Lock()
			swapped := len(b.buf) > 0 && b.buf[0] == 'b'
			b.mu.Unlock()
			if swapped {
				break
			}
			time.Sleep(time.Millisecond)
		}
		b.Write([]byte("cc\n"))
		close(wrote)
	}()
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("a Write that fits waited for the slow write")
	}
	close(g.open)
	<-flushing
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := g.got(), []string{"aaaa\n", "bbbb\ncc\n"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("writes = %q, want %q", got, want)
	}
}

// TestBufferedWriterKeepsSwapError checks that an error from writing out a
// full buffer, which the Write that swapped it cannot return, comes back
// from the next Write.
func TestBufferedWriterKeepsSwapError(t *testing.T) {
	r := recorder{err: errors.New("disk full")}
	b := NewBufferedWriter(&r, 4, time.Hour)
	defer b.Close()
	b.Write([]byte("abc\n"))
	if _, err := b.Write([]byte("def\n")); err != nil {
		t.Fatalf("Write that swapped = %v, want it buffered", err)
	}
	if _, err := b.Write([]byte("g")); err == nil {
		t.Error("Write after a failed write-out succeeded, want the error")
	}
}

func TestBufferedWriterCloseTwice(t *testing.T) {
	b := NewBufferedWriter(&bytes.Buffer{}, 8, time.Hour)
	b.Close()
	if err := b.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

func BenchmarkBufferedWriter(b *testing.B) {
	w := NewBufferedWriter(discard{}, 64<<10, time.Hour)
	defer w.Close()
	line := []byte(strings.Repeat("x", 200) + "\n")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w.Write(line)
		}
	})
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
