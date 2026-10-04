// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/update"
)

// This file only ever *looks*: it asks GitHub which release is newest and remembers the answer so that status,
// doctor and the web interface can tell the user. Replacing a binary is the explicit `update` command's job and is
// deliberately unreachable from here (ADR-0021; an architecture test keeps internal/update/apply out of the daemon).

const (
	updateEvery      = 24 * time.Hour
	updateFirstDelay = 3 * time.Minute // after a fresh start, so a restart loop never turns into a request loop
	updateRetry      = 6 * time.Hour   // after a failed check; failures are silent
	updateRecheck    = time.Hour       // how often the loop re-reads the config and the schedule
)

// jittered spreads a duration by up to ±frac, so installations do not all call at the same minute.
func jittered(base time.Duration, frac float64, r float64) time.Duration {
	return base + time.Duration((r*2-1)*frac*float64(base))
}

// jitterRand is scheduling jitter, not security.
func jitterRand() float64 {
	// #nosec G404 -- spreading request times, not a secret
	return rand.Float64() //nolint:gosec // see #nosec above
}

func (d *Daemon) updateLoop(ctx context.Context) {
	for {
		wait := d.updateTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.Clock.After(wait):
		}
	}
}

// updateTick runs the check when it is due and returns how long to sleep before looking again.
func (d *Daemon) updateTick(ctx context.Context) time.Duration {
	if !d.live.Load().cfg.UpdateCheck() {
		if st := update.ReadState(d.StateDir); st.Latest != "" || !st.Next.IsZero() {
			_ = update.WriteState(d.StateDir, update.State{}) // opted out: forget the notice so status and doctor go quiet too
		}
		return updateRecheck
	}
	cur, err := update.ParseVersion(d.version())
	if err != nil {
		return updateEvery // a development build has nothing to compare
	}
	now := d.Clock.Now()
	st := update.ReadState(d.StateDir)
	if st.Next.IsZero() {
		st.Next = now.Add(jittered(updateFirstDelay, 0.5, jitterRand()))
		_ = update.WriteState(d.StateDir, st)
	}
	if now.Before(st.Next) {
		return min(st.Next.Sub(now), updateRecheck)
	}
	d.checkForUpdate(ctx, cur, st)
	return time.Minute
}

// checkForUpdate performs the one GET and records the outcome. A failure only moves the next attempt back.
func (d *Daemon) checkForUpdate(ctx context.Context, cur update.Version, st update.State) {
	now := d.Clock.Now()
	rs, err := update.Checker{HTTP: d.HTTP, API: d.UpdateAPI}.Releases(ctx)
	if err != nil {
		d.Log.Debug("update check failed", "err", err)
		st.Next = now.Add(jittered(updateRetry, 0.1, jitterRand()))
		_ = update.WriteState(d.StateDir, st)
		return
	}
	st.Checked = now
	st.Next = now.Add(jittered(updateEvery, 0.1, jitterRand()))
	rel, ver, err := update.Newest(rs, cur)
	switch {
	case errors.Is(err, update.ErrNoRelease):
		st.Latest, st.URL, st.Urgent, st.Published = "", "", false, time.Time{}
	case err == nil:
		st.Latest, st.URL, st.Urgent, st.Published = ver.String(), rel.URL, rel.Urgent(), rel.Published
	}
	if st.Available(cur.String()) && st.Notified != st.Latest {
		level, what := "info", "A newer version is available"
		if st.Urgent {
			level, what = "warn", "A newer version with a security fix is available"
		}
		d.event(ctx, level, 0, "update-available", fmt.Sprintf("%s: %s (running %s). Review it and update with: repo-keeper update", what, st.Latest, cur))
		st.Notified = st.Latest
	}
	if err := update.WriteState(d.StateDir, st); err != nil {
		d.Log.Debug("saving update state", "err", err)
	}
}

// updateNotice is what the web interface may show: only when the check is on, a newer release is known, and its link
// is the project's own GitHub page.
func (d *Daemon) updateNotice() (version, url string, urgent bool) {
	l := d.live.Load()
	if l == nil || !l.cfg.UpdateCheck() {
		return "", "", false
	}
	st := update.ReadState(d.StateDir)
	if !st.Available(d.version()) {
		return "", "", false
	}
	if !strings.HasPrefix(st.URL, "https://github.com/") {
		st.URL = ""
	}
	return st.Latest, st.URL, st.Urgent
}
