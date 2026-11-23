// Package clock provides a simulated, deterministic virtual clock with a
// min-heap timer queue, so multi-day network simulations can run far faster
// than wall-clock time while preserving event ordering.
package clock

import (
	"container/heap"
	"sync"
	"time"
)

// Clock is a virtual clock. All goroutines that take part in a simulation
// should schedule work through After; the driver pumps pending timers with
// RunUntil / Step.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	timers  timerHeap
	nextID  int64
	stopped bool
	cond    *sync.Cond
}

type timer struct {
	at time.Time
	fn func()
	id int64
}

type timerHeap []*timer

func (h timerHeap) Len() int           { return len(h) }
func (h timerHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h timerHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)        { *h = append(*h, x.(*timer)) }
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return t
}

// New creates a virtual clock starting at t0.
func New(t0 time.Time) *Clock {
	c := &Clock{now: t0}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Now returns the current simulated time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After schedules fn to run at or after d virtual time. It mirrors
// time.AfterFunc semantics but on the virtual clock.
func (c *Clock) After(d time.Duration, fn func()) {
	c.mu.Lock()
	c.nextID++
	t := &timer{at: c.now.Add(d), fn: fn, id: c.nextID}
	heap.Push(&c.timers, t)
	c.cond.Broadcast()
	c.mu.Unlock()
}

// At schedules fn at absolute virtual time t.
func (c *Clock) At(t time.Time, fn func()) {
	c.mu.Lock()
	c.nextID++
	heap.Push(&c.timers, &timer{at: t, fn: fn, id: c.nextID})
	c.cond.Broadcast()
	c.mu.Unlock()
}

// Pending reports how many timers are queued.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timers.Len()
}

// Step fires every timer due before or at t, advancing the clock to t.
// Timers scheduled during execution for times <= t are also fired.
func (c *Clock) Step(t time.Time) int {
	fired := 0
	for {
		c.mu.Lock()
		if c.timers.Len() == 0 {
			c.now = t
			c.cond.Broadcast()
			c.mu.Unlock()
			return fired
		}
		next := c.timers[0]
		if next.at.After(t) {
			c.now = t
			c.cond.Broadcast()
			c.mu.Unlock()
			return fired
		}
		heap.Pop(&c.timers)
		c.now = next.at
		fn := next.fn
		c.mu.Unlock()
		fired++
		fn()
	}
}

// Next advances to the next timer and fires it. Returns false if none remain.
func (c *Clock) Next() bool {
	c.mu.Lock()
	if c.timers.Len() == 0 {
		c.mu.Unlock()
		return false
	}
	next := heap.Pop(&c.timers).(*timer)
	c.now = next.at
	fn := next.fn
	c.mu.Unlock()
	fn()
	return true
}

// Stop marks the clock stopped; waiting goroutines are released.
func (c *Clock) Stop() {
	c.mu.Lock()
	c.stopped = true
	c.cond.Broadcast()
	c.mu.Unlock()
}
