// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/store"
)

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func TestNext_Policy(t *testing.T) {
	const iv = time.Hour
	mid := 0.5 // jitter 0.5 => exactly the nominal delay
	tests := []struct {
		name     string
		k        Kind
		failures int
		retryAt  time.Time
		wantWait time.Duration
		wantF    int
		wantAttn bool
	}{
		{"success", Success, 3, time.Time{}, iv, 0, false},
		{"busy", Busy, 2, time.Time{}, time.Minute, 2, false},
		{"transient first", Transient, 0, time.Time{}, 2 * time.Minute, 1, false},
		{"transient third", Transient, 2, time.Time{}, 8 * time.Minute, 3, false},
		{"transient capped at interval", Transient, 10, time.Time{}, iv, 11, true},
		{"flag after five", Transient, 4, time.Time{}, 32 * time.Minute, 5, true},
		{"rate limited", RateLimited, 1, t0.Add(10 * time.Minute), 10*time.Minute + 30*time.Second, 1, false},
		{"rate limited in the past", RateLimited, 0, t0.Add(-time.Hour), 30 * time.Second, 0, false},
		{"needs user", NeedsUser, 0, time.Time{}, 6 * time.Hour, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Next(t0, iv, tc.failures, tc.k, tc.retryAt, mid)
			if d.Next.Sub(t0) != tc.wantWait || d.Failures != tc.wantF || d.Attention != tc.wantAttn {
				t.Fatalf("got wait=%v %+v", d.Next.Sub(t0), d)
			}
		})
	}
}

func TestNext_JitterStaysWithin20Percent(t *testing.T) {
	for _, j := range []float64{0, 0.25, 0.999} {
		w := Next(t0, time.Hour, 0, Success, time.Time{}, j).Next.Sub(t0)
		if w < 48*time.Minute || w > 72*time.Minute {
			t.Fatalf("jitter %v => %v", j, w)
		}
	}
}

type rig struct {
	s   *store.Store
	clk *clock.Fake
	sc  *Scheduler
	run chan store.Repo
	cnt atomic.Int32
}

func newRig(t *testing.T, workers int, job func(r store.Repo) Result) *rig {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &rig{s: s, clk: clock.NewFake(t0), run: make(chan store.Repo, 100)}
	r.sc = &Scheduler{
		Store: s, Clock: r.clk, Tick: time.Minute, Workers: workers, Jitter: func() float64 { return 0.5 },
		Job: func(_ context.Context, repo store.Repo) Result {
			r.cnt.Add(1)
			r.run <- repo
			return job(repo)
		},
	}
	return r
}

func okResult(store.Repo) Result { return Result{Run: store.Run{Status: "ok"}, Kind: Success} }

func (r *rig) start(t *testing.T) (stop func()) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = r.sc.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func (r *rig) wait(t *testing.T, n int) []store.Repo {
	t.Helper()
	var got []store.Repo
	for range n {
		select {
		case repo := <-r.run:
			got = append(got, repo)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out after %d/%d jobs", len(got), n)
		}
	}
	return got
}

func TestRun_DispatchesDueRepoThenReschedules(t *testing.T) {
	r := newRig(t, 2, okResult)
	_ = r.s.SyncRepos(t.Context(), []store.Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, func(int) time.Time { return t0 })
	defer r.start(t)()

	r.wait(t, 1)
	r.settle(t)
	repos, _ := r.s.ListRepos(t.Context())
	if !repos[0].NextSync.Equal(t0.Add(time.Hour)) || repos[0].LastStatus != "ok" {
		t.Fatalf("repo=%+v", repos[0])
	}

	r.clk.Advance(30 * time.Minute) // not due yet
	r.sc.Wake()                     // abandoned fake timers make "a waiter exists" meaningless; dispatch explicitly against the new time
	time.Sleep(50 * time.Millisecond)
	if r.cnt.Load() != 1 {
		t.Fatal("ran before due")
	}
	r.clk.Advance(31 * time.Minute)
	r.sc.Wake() // abandoned fake timers make "a waiter exists" meaningless; dispatch explicitly against the new time
	r.wait(t, 1)
}

func TestRun_MissedWhileAsleep_RunsOnceNotMany(t *testing.T) {
	r := newRig(t, 2, okResult)
	_ = r.s.SyncRepos(t.Context(), []store.Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, func(int) time.Time { return t0 })
	defer r.start(t)()
	r.wait(t, 1)
	r.settle(t)
	r.clk.Advance(10 * time.Hour) // laptop slept through ten intervals
	r.sc.Wake()                   // abandoned fake timers make "a waiter exists" meaningless; dispatch explicitly against the new time
	r.wait(t, 1)
	time.Sleep(50 * time.Millisecond)
	if r.cnt.Load() != 2 {
		t.Fatalf("runs=%d, want exactly 2", r.cnt.Load())
	}
}

func TestRun_WorkerLimitRespected(t *testing.T) {
	var cur, peak atomic.Int32
	release := make(chan struct{})
	r := newRig(t, 2, func(store.Repo) Result {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		cur.Add(-1)
		return okResult(store.Repo{})
	})
	specs := []store.Spec{{Path: "/a", Remote: "o", Interval: time.Hour}, {Path: "/b", Remote: "o", Interval: time.Hour}, {Path: "/c", Remote: "o", Interval: time.Hour}}
	_ = r.s.SyncRepos(t.Context(), specs, func(int) time.Time { return t0 })
	defer r.start(t)()
	r.wait(t, 2)
	time.Sleep(50 * time.Millisecond)
	if cur.Load() != 2 {
		t.Fatalf("in flight=%d", cur.Load())
	}
	close(release)
	r.wait(t, 1)
	if peak.Load() > 2 {
		t.Fatalf("peak=%d", peak.Load())
	}
}

func TestRun_QuietHours_PausesScheduledButNotManual(t *testing.T) {
	r := newRig(t, 2, okResult)
	r.sc.Quiet = func(time.Time) bool { return true }
	_ = r.s.SyncRepos(t.Context(), []store.Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, func(int) time.Time { return t0 })
	defer r.start(t)()
	time.Sleep(80 * time.Millisecond)
	if r.cnt.Load() != 0 {
		t.Fatal("quiet hours ignored")
	}
	repos, _ := r.s.ListRepos(t.Context())
	if err := r.sc.TriggerNow(t.Context(), repos[0].ID); err != nil {
		t.Fatal(err)
	}
	r.wait(t, 1)
}

func TestRun_RateLimitedJob_DefersWithoutRecordingRun(t *testing.T) {
	r := newRig(t, 1, func(store.Repo) Result {
		return Result{Kind: RateLimited, RetryAt: t0.Add(20 * time.Minute), NoRecord: true}
	})
	_ = r.s.SyncRepos(t.Context(), []store.Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, func(int) time.Time { return t0 })
	defer r.start(t)()
	r.wait(t, 1)
	r.settle(t)
	repos, _ := r.s.ListRepos(t.Context())
	runs, _ := r.s.RecentRuns(t.Context(), repos[0].ID, 5)
	if len(runs) != 0 || repos[0].NextSync.Before(t0.Add(20*time.Minute)) || repos[0].Failures != 0 {
		t.Fatalf("runs=%d repo=%+v", len(runs), repos[0])
	}
}

func TestRun_RepeatedFailures_RaiseAttentionEventOnce(t *testing.T) {
	r := newRig(t, 1, func(store.Repo) Result {
		return Result{Run: store.Run{Status: "failed", Reason: "fetch", Error: "boom"}, Kind: Transient}
	})
	_ = r.s.SyncRepos(t.Context(), []store.Spec{{Path: "/a", Remote: "o", Interval: 10 * time.Minute}}, func(int) time.Time { return t0 })
	defer r.start(t)()
	for i := 0; i < attentionAfter+2; i++ {
		r.wait(t, 1)
		r.settle(t)
		r.clk.Advance(30 * time.Minute)
		r.sc.Wake() // abandoned fake timers make "a waiter exists" meaningless; dispatch explicitly against the new time
	}
	repos, _ := r.s.ListRepos(t.Context())
	if !repos[0].NeedsAttention || repos[0].Failures < attentionAfter {
		t.Fatalf("repo=%+v", repos[0])
	}
	var attn int
	evs, _ := r.s.RecentEvents(t.Context(), 100)
	for _, e := range evs {
		if e.Code == "needs-attention" {
			attn++
		}
	}
	if attn != 1 {
		t.Fatalf("attention events=%d, want 1 (%+v)", attn, evs)
	}
}

func TestRun_Shutdown_WaitsForJobAndDoesNotRecordIt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r := newRig(t, 1, func(store.Repo) Result {
		once.Do(func() { close(started) })
		<-release
		return okResult(store.Repo{})
	})
	_ = r.s.SyncRepos(t.Context(), []store.Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, func(int) time.Time { return t0 })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = r.sc.Run(ctx); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned before the in-flight job finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	repos, _ := r.s.ListRepos(t.Context())
	if repos[0].LastStatus != "" {
		t.Fatalf("cancelled run was recorded: %+v", repos[0])
	}
}

func TestRun_Paused_StopsScheduledButNotManual_AndResumeWakes(t *testing.T) {
	r := newRig(t, 2, okResult)
	_ = r.s.SyncRepos(t.Context(), []store.Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, func(int) time.Time { return t0 })
	r.sc.SetPaused(true)
	defer r.start(t)()
	time.Sleep(80 * time.Millisecond)
	if r.cnt.Load() != 0 || !r.sc.Paused() {
		t.Fatal("paused scheduler dispatched work")
	}
	repos, _ := r.s.ListRepos(t.Context())
	if err := r.sc.TriggerNow(t.Context(), repos[0].ID); err != nil {
		t.Fatal(err)
	}
	r.wait(t, 1)
	r.settle(t)
	r.clk.Advance(2 * time.Hour)
	r.sc.Wake() // abandoned fake timers make "a waiter exists" meaningless; dispatch explicitly against the new time
	time.Sleep(50 * time.Millisecond)
	if r.cnt.Load() != 1 {
		t.Fatal("paused scheduler ran a scheduled sync")
	}
	r.sc.SetPaused(false) // resuming picks up the overdue repo without waiting for the next tick
	r.wait(t, 1)
}

// settle waits until no job is in flight, i.e. its result is recorded. Waiting on
// the clock alone is not enough: the next tick is registered while the job is still running.
func (r *rig) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.sc.mu.Lock()
		n := len(r.sc.inflight)
		r.sc.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
}
