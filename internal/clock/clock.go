// SPDX-License-Identifier: Apache-2.0

// Package clock abstracts time so schedulers and rate limiters are testable without sleeping.
package clock

import (
	"sync"
	"time"
)

// Clock is the time source injected into anything time-dependent.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// Real is the wall-clock implementation.
type Real struct{}

// Now implements Clock.
func (Real) Now() time.Time { return time.Now() }

// After implements Clock.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a manually advanced Clock for tests.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewFake returns a Fake starting at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now implements Clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After implements Clock; the channel fires on the Advance that reaches d.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, waiter{at: f.now.Add(d), ch: ch})
	return ch
}

// Advance moves time forward by d and fires every waiter that is now due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	kept := f.waiters[:0]
	for _, w := range f.waiters {
		if w.at.After(f.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- f.now
	}
	f.waiters = kept
}

// Waiters returns how many After channels are still pending (lets tests sync with goroutines).
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntil spins until at least n waiters are registered or the deadline passes; it reports success.
func (f *Fake) BlockUntil(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.Waiters() >= n {
			return true
		}
		time.Sleep(time.Millisecond) //nolint:forbidigo // real-time poll for test synchronisation only
	}
	return false
}
