package resilience

import (
	"sync"
	"time"
)

// BudgetWindow is the span a Budget counts requests and retries over, as in
// Finagle.
const BudgetWindow = 10 * time.Second

// Budget caps a route's retries at a share of its recent requests plus a
// small floor, so that when every backend is failing, retries add at most
// that share to the load instead of multiplying it by the retry count.
//
// It counts every attempt in a Window and marks the retries, so requests
// are Total − Marked.
type Budget struct {
	ratio   float64
	reserve float64 // retries allowed in a window with no requests at all

	mu sync.Mutex
	w  *Window

	now func() time.Time // replaced by tests
}

// NewBudget returns a Budget that allows ratio retries per request, plus
// minPerSec retries per second whatever the traffic, so a quiet route can
// still retry.
func NewBudget(ratio float64, minPerSec int) *Budget {
	return &Budget{
		ratio:   ratio,
		reserve: float64(minPerSec) * BudgetWindow.Seconds(),
		w:       NewWindow(BudgetWindow, time.Now()),
		now:     time.Now,
	}
}

// Request records a request's first attempt, which earns ratio retries.
func (b *Budget) Request() {
	now := b.now()
	b.mu.Lock()
	b.w.Add(now, Counts{Total: 1})
	b.mu.Unlock()
}

// TryRetry reports whether one more retry fits the budget, and records it if
// it does.
func (b *Budget) TryRetry() bool {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.w.Sum(now)
	if float64(c.Marked+1) > b.ratio*float64(c.Total-c.Marked)+b.reserve {
		return false
	}
	b.w.Add(now, Counts{Total: 1, Marked: 1})
	return true
}
