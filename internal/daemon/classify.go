// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/sched"
	"github.com/basmulder03/repo-keeper/internal/syncer"
)

// hostOf extracts the host from https/ssh/scp-style git URLs; local paths map to "local".
func hostOf(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return "local"
	}
	if strings.Contains(u, "://") {
		if p, err := url.Parse(u); err == nil && p.Hostname() != "" {
			return strings.ToLower(p.Host)
		}
		return "local"
	}
	// scp-like: [user@]host:path (but not a Windows drive letter such as C:\repo)
	if at := strings.IndexByte(u, '@'); at >= 0 || (strings.Contains(u, ":") && !strings.HasPrefix(u, "/") && !isDriveLetter(u)) {
		rest := u
		if at >= 0 {
			rest = u[at+1:]
		}
		if h, _, ok := strings.Cut(rest, ":"); ok && h != "" {
			return strings.ToLower(h)
		}
	}
	return "local"
}

func isDriveLetter(s string) bool {
	return len(s) >= 2 && s[1] == ':' && (s[0]|0x20) >= 'a' && (s[0]|0x20) <= 'z'
}

type verdict struct {
	kind sched.Kind
	// resp feeds the rate limiter.
	resp ratelimit.Response
}

var (
	rateLimitMarks = []string{"rate limit", "too many requests", "http 429", "error: 429", "returned error: 429", "abuse detection", "secondary rate"}
	authMarks      = []string{
		"authentication failed", "could not read username", "could not read password", "terminal prompts disabled",
		"permission denied (publickey", "invalid credentials", "returned error: 401", "returned error: 403",
		"repository not found", "access denied", "bad credentials",
	}
	networkReasons = map[string]bool{"ls-remote": true, "fetch": true, "clone": true}
)

// classify maps a sync result to a scheduling kind and a limiter observation.
func classify(r syncer.Result) verdict {
	switch r.Status {
	case syncer.OK:
		return verdict{kind: sched.Success, resp: ratelimit.Response{Status: 200}}
	case syncer.Skipped:
		if r.Reason == "locked" {
			return verdict{kind: sched.Busy, resp: ratelimit.Response{Status: 200}}
		}
		return verdict{kind: sched.Success, resp: ratelimit.Response{Status: 200}}
	}
	if r.Reason == "unsafe-config" {
		return verdict{kind: sched.NeedsUser, resp: ratelimit.Response{Status: 200}}
	}
	msg := strings.ToLower(errText(r.Err))
	if !networkReasons[r.Reason] {
		return verdict{kind: sched.Transient, resp: ratelimit.Response{Status: 200}} // local failure, not the remote's fault
	}
	for _, m := range rateLimitMarks {
		if strings.Contains(msg, m) {
			return verdict{kind: sched.RateLimited, resp: ratelimit.Response{RateLimited: true, RetryAfter: 0}}
		}
	}
	for _, m := range authMarks {
		if strings.Contains(msg, m) {
			return verdict{kind: sched.NeedsUser, resp: ratelimit.Response{Status: 200}} // our credentials, not host health
		}
	}
	return verdict{kind: sched.Transient, resp: ratelimit.Response{Err: r.Err}}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	var ge *gitx.Error
	if errors.As(err, &ge) {
		return ge.Stderr
	}
	return err.Error()
}

// retryAtFrom turns a limiter cooldown into the scheduler's RetryAt (falls back to a minute).
func retryAtFrom(cooldown, now time.Time) time.Time {
	if cooldown.After(now) {
		return cooldown
	}
	return now.Add(time.Minute)
}
