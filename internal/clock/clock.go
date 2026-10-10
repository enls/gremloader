// Package clock makes time an injected dependency. Anything that decides based on "now"
// (schedules, cool-down, stale-lock detection, retry delays) takes a Clock instead of calling
// time.Now, so tests can freeze and advance time.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}

// System is the real wall clock.
type System struct{}

// Now returns the current local time.
func (System) Now() time.Time { return time.Now() }

// Fake is a clock that only moves when told to. It is safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake frozen at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now returns the frozen time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward (or backward, for a negative duration).
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set jumps the clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
