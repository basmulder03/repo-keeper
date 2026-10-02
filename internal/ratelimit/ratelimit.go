// SPDX-License-Identifier: Apache-2.0

// Package ratelimit keeps repo-keeper polite: per-host pacing, concurrency caps, cooldowns and a circuit breaker.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
)

// Config tunes one limiter; zero values get safe defaults.
type Config struct {
	// Rate is sustained requests per second per host (default 1).
	Rate float64
	// Burst is the token bucket size (default 5).
	Burst int
	// MaxConcurrent caps in-flight operations per host (default 2).
	MaxConcurrent int
	// BackoffBase and BackoffCap bound the exponential failure backoff (defaults 30s, 1h).
	BackoffBase, BackoffCap time.Duration
	// BreakerThreshold is the consecutive failures that open the circuit (default 5).
	BreakerThreshold int
}

func (c *Config) defaults() {
	if c.Rate <= 0 {
		c.Rate = 1
	}
	if c.Burst <= 0 {
		c.Burst = 5
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 2
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 30 * time.Second
	}
	if c.BackoffCap <= 0 {
		c.BackoffCap = time.Hour
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 5
	}
}

// WaitError is returned when the required wait exceeds the caller's maximum; retry at RetryAt.
type WaitError struct {
	Host    string
	RetryAt time.Time
	Reason  string
}

// Error implements error.
func (e *WaitError) Error() string {
	return fmt.Sprintf("ratelimit: %s unavailable until %s (%s)", e.Host, e.RetryAt.Format(time.RFC3339), e.Reason)
}

// ErrNoQuota is returned by Budget checks when the remaining quota is below the requested reserve.
var ErrNoQuota = errors.New("ratelimit: quota reserve reached")

// Limiter holds per-host state.
type Limiter struct {
	cfg   Config
	clk   clock.Clock
	rnd   func() float64 // [0,1) for jitter; injectable for tests
	mu    sync.Mutex
	hosts map[string]*host
}

type host struct {
	sem        chan struct{}
	tokens     float64
	last       time.Time
	cooldown   time.Time // no requests before this instant
	cooldownBy string
	failures   int
	open       bool // circuit open: only the post-cooldown probe may pass
	limit      int  // last seen quota (0 = unknown)
	remaining  int
	reset      time.Time
}

// New builds a Limiter. rnd may be nil (uses a fixed 0.5 jitter factor; production passes rand.Float64).
func New(cfg Config, clk clock.Clock, rnd func() float64) *Limiter {
	cfg.defaults()
	if rnd == nil {
		rnd = func() float64 { return 0.5 }
	}
	return &Limiter{cfg: cfg, clk: clk, rnd: rnd, hosts: map[string]*host{}}
}

func (l *Limiter) get(name string) *host {
	h, ok := l.hosts[name]
	if !ok {
		h = &host{sem: make(chan struct{}, l.cfg.MaxConcurrent), tokens: float64(l.cfg.Burst), last: l.clk.Now()}
		l.hosts[name] = h
	}
	return h
}

// Permit is a held slot; call Release exactly once with the outcome.
type Permit struct {
	l    *Limiter
	name string
	once sync.Once
}

// Acquire blocks until host may be contacted, or returns *WaitError if that takes longer than maxWait
// (maxWait <= 0 means wait as long as ctx allows).
func (l *Limiter) Acquire(ctx context.Context, name string, maxWait time.Duration) (*Permit, error) {
	l.mu.Lock()
	h := l.get(name)
	l.mu.Unlock()

	for {
		l.mu.Lock()
		wait, reason := l.waitFor(h)
		if wait <= 0 {
			h.tokens--
			l.mu.Unlock()
			break
		}
		at := l.clk.Now().Add(wait)
		l.mu.Unlock()
		if maxWait > 0 && wait > maxWait {
			return nil, &WaitError{Host: name, RetryAt: at, Reason: reason}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.clk.After(wait):
		}
	}

	select {
	case h.sem <- struct{}{}:
		return &Permit{l: l, name: name}, nil
	case <-ctx.Done():
		l.mu.Lock()
		h.tokens++ // give the unused token back
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

// waitFor returns how long until a token is available (caller holds l.mu).
func (l *Limiter) waitFor(h *host) (time.Duration, string) {
	now := l.clk.Now()
	if el := now.Sub(h.last); el > 0 {
		h.tokens = math.Min(float64(l.cfg.Burst), h.tokens+el.Seconds()*l.cfg.Rate)
		h.last = now
	}
	if now.Before(h.cooldown) {
		return h.cooldown.Sub(now), h.cooldownBy
	}
	if h.tokens >= 1 {
		return 0, ""
	}
	return time.Duration((1 - h.tokens) / l.cfg.Rate * float64(time.Second)), "pacing"
}

// Response is what a Permit needs to learn from an operation.
type Response struct {
	Status int         // HTTP status; 0 for non-HTTP (git) operations
	Header http.Header // may be nil
	Err    error       // transport/git failure
	// RateLimited marks non-HTTP operations (git) that were throttled by the remote.
	RateLimited bool
	// RetryAfter overrides header parsing when set (git has no headers).
	RetryAfter time.Duration
}

// Release frees the slot and feeds the outcome into cooldown, breaker and quota tracking.
func (p *Permit) Release(r Response) {
	p.once.Do(func() {
		l := p.l
		l.mu.Lock()
		defer l.mu.Unlock()
		h := l.get(p.name)
		<-h.sem
		l.observe(h, r)
	})
}

func (l *Limiter) observe(h *host, r Response) {
	now := l.clk.Now()
	if r.Header != nil {
		l.readQuota(h, r.Header, now)
	}
	throttled := r.RateLimited || r.Status == http.StatusTooManyRequests ||
		(r.Status == http.StatusForbidden && (h.exhausted() || r.Header.Get("Retry-After") != ""))
	retry := r.RetryAfter
	if retry == 0 && r.Header != nil {
		retry = parseRetryAfter(r.Header.Get("Retry-After"), now)
	}

	switch {
	case throttled:
		// Server-directed waits are honoured as given; our own jittered backoff applies only when it gave none.
		d := retry
		if d <= 0 {
			if h.exhausted() {
				d = h.reset.Sub(now)
			} else {
				h.failures++
				d = l.backoff(h.failures)
			}
		}
		l.setCooldown(h, now.Add(d), "rate-limited")
	case r.Err != nil || r.Status >= 500:
		h.failures++
		d := l.backoff(h.failures)
		if h.failures >= l.cfg.BreakerThreshold {
			h.open = true
			l.setCooldown(h, now.Add(d), "circuit-open")
		} else {
			l.setCooldown(h, now.Add(d), "backoff")
		}
	default:
		h.failures, h.open = 0, false
		if h.exhausted() {
			l.setCooldown(h, h.reset, "quota-exhausted")
		}
	}
}

func (h *host) exhausted() bool { return h.limit > 0 && h.remaining <= 0 && !h.reset.IsZero() }

func (l *Limiter) setCooldown(h *host, until time.Time, why string) {
	if until.After(h.cooldown) {
		h.cooldown, h.cooldownBy = until, why
	}
}

// backoff is exponential with full jitter in [d/2, d] so synchronized clients spread out.
func (l *Limiter) backoff(failures int) time.Duration {
	d := float64(l.cfg.BackoffBase) * math.Pow(2, float64(failures-1))
	d = math.Min(d, float64(l.cfg.BackoffCap))
	return time.Duration(d/2 + l.rnd()*d/2)
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// readQuota understands GitHub/GitLab/IETF style headers.
func (l *Limiter) readQuota(h *host, hdr http.Header, now time.Time) {
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := hdr.Get(k); v != "" {
				return v
			}
		}
		return ""
	}
	if v, err := strconv.Atoi(first("X-RateLimit-Limit", "RateLimit-Limit")); err == nil {
		h.limit = v
	}
	rem, err := strconv.Atoi(first("X-RateLimit-Remaining", "RateLimit-Remaining"))
	if err != nil {
		return
	}
	h.remaining = rem
	if v, err := strconv.ParseInt(first("X-RateLimit-Reset", "RateLimit-Reset"), 10, 64); err == nil && v > 0 {
		if v > 1e9 { // epoch seconds (GitHub, GitLab); otherwise IETF delta seconds
			h.reset = time.Unix(v, 0)
		} else {
			h.reset = now.Add(time.Duration(v) * time.Second)
		}
		if h.limit == 0 {
			h.limit = rem + 1 // header gave a remaining count without a limit; mark quota as known
		}
	}
}

// Quota is a host's last known budget.
type Quota struct {
	Limit, Remaining int
	Reset            time.Time
}

// Budget reports the last known quota for host (Limit 0 = unknown).
func (l *Limiter) Budget(name string) Quota {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.get(name)
	return Quota{Limit: h.limit, Remaining: h.remaining, Reset: h.reset}
}

// HasReserve reports whether more than reserve (0..1) of the quota remains; unknown quota counts as yes.
func (l *Limiter) HasReserve(name string, reserve float64) bool {
	q := l.Budget(name)
	if q.Limit == 0 || !q.Reset.After(l.clk.Now()) {
		return true
	}
	return float64(q.Remaining) > reserve*float64(q.Limit)
}

// HostState is a snapshot for the UI.
type HostState struct {
	Host        string
	Cooldown    time.Time // zero if none
	CooldownBy  string
	Failures    int
	CircuitOpen bool
	InFlight    int
	Quota       Quota
}

// Snapshot returns the state of every known host, sorted by name.
func (l *Limiter) Snapshot() []HostState {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	var out []HostState
	for name, h := range l.hosts {
		s := HostState{Host: name, Failures: h.failures, CircuitOpen: h.open, InFlight: len(h.sem),
			Quota: Quota{Limit: h.limit, Remaining: h.remaining, Reset: h.reset}}
		if now.Before(h.cooldown) {
			s.Cooldown, s.CooldownBy = h.cooldown, h.cooldownBy
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// CooldownUntil returns when host may next be contacted (zero if no cooldown is active).
func (l *Limiter) CooldownUntil(name string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.get(name)
	if l.clk.Now().Before(h.cooldown) {
		return h.cooldown
	}
	return time.Time{}
}
