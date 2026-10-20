package resilience

import "time"

// windowBuckets is how many buckets a Window splits its span into. More
// buckets slide more smoothly but cost more to clear after an idle gap.
const windowBuckets = 10

// Counts is what a Window holds: a total and a marked subset of it, such as
// requests and the ones that failed, or attempts and the ones that were
// retries.
type Counts struct {
	Total  int64
	Marked int64
}

func (c *Counts) add(d Counts) {
	c.Total += d.Total
	c.Marked += d.Marked
}

func (c *Counts) sub(d Counts) {
	c.Total -= d.Total
	c.Marked -= d.Marked
}

// Window counts events over a sliding span of time, kept as windowBuckets
// buckets with a running sum. Sum covers the current bucket and the
// windowBuckets-1 before it, so it sees between 9/10 of the span and all of
// it.
//
// A Window is not safe for concurrent use. Its owner, such as a circuit
// breaker, holds its own lock around the window and its other state.
type Window struct {
	width   time.Duration
	origin  time.Time
	head    int64 // number of the newest bucket, counted in widths from origin
	buckets [windowBuckets]Counts
	sum     Counts
}

// NewWindow returns an empty window over span, starting at now. Times passed
// to its methods are measured from now with time.Time.Sub, so the monotonic
// clock keeps wall clock jumps out of the counts.
func NewWindow(span time.Duration, now time.Time) *Window {
	return &Window{width: max(span/windowBuckets, 1), origin: now}
}

// Add counts c at time now.
func (w *Window) Add(now time.Time, c Counts) {
	w.advance(now)
	w.buckets[w.head%windowBuckets].add(c)
	w.sum.add(c)
}

// Sum returns the counts in the window that ends at now.
func (w *Window) Sum(now time.Time) Counts {
	w.advance(now)
	return w.sum
}

// Reset empties the window.
func (w *Window) Reset() {
	w.buckets = [windowBuckets]Counts{}
	w.sum = Counts{}
}

// advance moves the head to the bucket holding now, clearing every bucket it
// passes over. A time older than the head counts in the head bucket, so a
// caller that read the clock before a concurrent one still lands in range.
func (w *Window) advance(now time.Time) {
	n := int64(now.Sub(w.origin) / w.width)
	if n <= w.head {
		return
	}
	if n-w.head >= windowBuckets {
		w.Reset()
		w.head = n
		return
	}
	for w.head < n {
		w.head++
		b := &w.buckets[w.head%windowBuckets]
		w.sum.sub(*b)
		*b = Counts{}
	}
}
