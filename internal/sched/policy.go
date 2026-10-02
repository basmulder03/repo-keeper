// SPDX-License-Identifier: Apache-2.0

package sched

import "time"

// Kind classifies a finished job for scheduling purposes.
type Kind uint8

// Job result kinds.
const (
	Success     Kind = iota // synced (including benign skips such as a dirty tree)
	Busy                    // repo locked by another operation: try again shortly
	Transient               // network/remote hiccup: back off
	RateLimited             // remote throttled us: wait until RetryAt
	NeedsUser               // auth/config problem a human must fix: slow retries, flag it
)

const (
	busyRetry      = time.Minute
	transientBase  = 2 * time.Minute
	needsUserRetry = 6 * time.Hour
	attentionAfter = 5 // consecutive transient failures before flagging
	jitterFraction = 0.2
)

// Decision is what happens to a repo after a job.
type Decision struct {
	Next      time.Time
	Failures  int
	Attention bool
}

// Next is the single place where retry and backoff policy lives.
// jitter is a uniform [0,1) sample; retryAt is only used for RateLimited.
func Next(now time.Time, interval time.Duration, failures int, k Kind, retryAt time.Time, jitter float64) Decision {
	spread := func(d time.Duration) time.Duration { // d * (1 ± 20 %)
		return time.Duration(float64(d) * (1 + (jitter*2-1)*jitterFraction))
	}
	switch k {
	case Success:
		return Decision{Next: now.Add(spread(interval))}
	case Busy:
		return Decision{Next: now.Add(busyRetry), Failures: failures}
	case RateLimited:
		next := retryAt
		if next.Before(now) {
			next = now
		}
		return Decision{Next: next.Add(time.Duration(jitter * float64(time.Minute))), Failures: failures}
	case NeedsUser:
		wait := needsUserRetry
		if interval > wait {
			wait = interval
		}
		return Decision{Next: now.Add(wait), Failures: failures + 1, Attention: true}
	default: // Transient
		f := failures + 1
		wait := transientBase
		for i := 1; i < f && wait < interval; i++ {
			wait *= 2
		}
		if wait > interval {
			wait = interval
		}
		return Decision{Next: now.Add(spread(wait)), Failures: f, Attention: f >= attentionAfter}
	}
}
