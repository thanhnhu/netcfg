// Package clock provides the real implementation of ports.Clock and a fake one
// so commit-confirm timers can be tested without waiting in real time.
package clock

import (
	"sync"
	"time"

	"netcfg/internal/ports"
)

// Real delegates to the standard library.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) AfterFunc(d time.Duration, f func()) ports.Timer { return time.AfterFunc(d, f) }

// Fake advances only when a test tells it to.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	fn      func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// NewFake starts a fake clock at the given instant.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) AfterFunc(d time.Duration, fn func()) ports.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()

	t := &fakeTimer{at: f.now.Add(d), fn: fn}
	f.timers = append(f.timers, t)
	return t
}

// Advance moves time forward and fires every timer that became due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now

	var due []*fakeTimer
	remaining := f.timers[:0]
	for _, t := range f.timers {
		switch {
		case t.stopped:
		case !t.at.After(now):
			due = append(due, t)
		default:
			remaining = append(remaining, t)
		}
	}
	f.timers = remaining
	f.mu.Unlock()

	for _, t := range due {
		t.fn()
	}
}
