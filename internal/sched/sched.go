// SPDX-License-Identifier: Apache-2.0

// Package sched decides when each repository is synced and runs jobs on a bounded worker pool.
package sched

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/store"
)

// Store is the persistence the scheduler needs; *store.Store implements it.
type Store interface {
	Due(ctx context.Context, now time.Time, limit int) ([]store.Repo, error)
	Record(ctx context.Context, r store.Run) error
	Defer(ctx context.Context, id int64, until time.Time) error
	AddEvent(ctx context.Context, e store.Event) error
}

// Result is a job's report; Run carries everything except the scheduling fields, which Next fills in.
type Result struct {
	Run     store.Run
	Kind    Kind
	RetryAt time.Time
	// NoRecord means the job did nothing (e.g. host cooling down): reschedule only.
	NoRecord bool
}

// Job syncs one repository.
type Job func(ctx context.Context, repo store.Repo) Result

// Scheduler dispatches due repositories.
type Scheduler struct {
	Store      Store
	Job        Job
	Clock      clock.Clock
	Log        *slog.Logger
	Tick       time.Duration // how often to look for due repos (default 30s)
	Workers    int           // parallel jobs (default 4)
	JobTimeout time.Duration // per job (default 20m)
	Jitter     func() float64
	// Quiet reports whether scheduled (not manual) syncs are paused at t.
	Quiet func(t time.Time) bool

	mu       sync.Mutex
	inflight map[int64]bool
	forced   map[int64]bool
	wake     chan struct{}
}

func (s *Scheduler) init() {
	if s.Tick <= 0 {
		s.Tick = 30 * time.Second
	}
	if s.Workers <= 0 {
		s.Workers = 4
	}
	if s.JobTimeout <= 0 {
		s.JobTimeout = 20 * time.Minute
	}
	if s.Jitter == nil {
		s.Jitter = rand.Float64
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight, s.forced, s.wake = map[int64]bool{}, map[int64]bool{}, make(chan struct{}, 1)
	}
	s.mu.Unlock()
}

// TriggerNow makes a repo due immediately and bypasses quiet hours (rate limits still apply).
func (s *Scheduler) TriggerNow(ctx context.Context, id int64) error {
	s.init()
	if err := s.Store.Defer(ctx, id, s.Clock.Now()); err != nil {
		return err
	}
	s.mu.Lock()
	s.forced[id] = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// Run blocks until ctx is cancelled, then waits for in-flight jobs to finish.
func (s *Scheduler) Run(ctx context.Context) error {
	s.init()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		s.dispatch(ctx, &wg)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.Clock.After(s.Tick):
		case <-s.wake:
		}
	}
}

func (s *Scheduler) dispatch(ctx context.Context, wg *sync.WaitGroup) {
	now := s.Clock.Now()
	due, err := s.Store.Due(ctx, now, s.Workers*4)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Error("scheduler: listing due repos", "err", err)
		}
		return
	}
	quiet := s.Quiet != nil && s.Quiet(now)
	for _, r := range due {
		s.mu.Lock()
		busy, forced := s.inflight[r.ID], s.forced[r.ID]
		full := len(s.inflight) >= s.Workers
		if busy || full || (quiet && !forced) {
			s.mu.Unlock()
			if full {
				return
			}
			continue
		}
		s.inflight[r.ID] = true
		delete(s.forced, r.ID)
		s.mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runOne(ctx, r)
			s.mu.Lock()
			delete(s.inflight, r.ID)
			s.mu.Unlock()
			select { // freed a slot: look for more work right away
			case s.wake <- struct{}{}:
			default:
			}
		}()
	}
}

func (s *Scheduler) runOne(ctx context.Context, r store.Repo) {
	jctx, cancel := context.WithTimeout(ctx, s.JobTimeout)
	defer cancel()
	res := s.Job(jctx, r)
	if ctx.Err() != nil {
		return // shutting down: leave the repo due instead of recording a cancelled run
	}
	now := s.Clock.Now()
	// Bookkeeping must survive a cancelled parent, so it uses a fresh bounded context.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer bcancel()

	if res.NoRecord {
		d := Next(now, r.Interval, r.Failures, res.Kind, res.RetryAt, s.Jitter())
		if err := s.Store.Defer(bctx, r.ID, d.Next); err != nil {
			s.Log.Error("scheduler: defer", "repo", r.Path, "err", err)
		}
		return
	}
	d := Next(now, r.Interval, r.Failures, res.Kind, res.RetryAt, s.Jitter())
	run := res.Run
	run.RepoID, run.NextSync, run.Failures, run.NeedsAttention = r.ID, d.Next, d.Failures, d.Attention
	if err := s.Store.Record(bctx, run); err != nil {
		s.Log.Error("scheduler: recording run", "repo", r.Path, "err", err)
		return
	}
	s.event(bctx, r, run, d)
}

// event logs state changes worth a human's attention.
func (s *Scheduler) event(ctx context.Context, r store.Repo, run store.Run, d Decision) {
	level, code, msg := "", "", ""
	switch {
	case d.Attention && !r.NeedsAttention:
		level, code, msg = "error", "needs-attention", run.Reason+": "+run.Error
	case run.Status == "failed" && r.Failures == 0:
		level, code, msg = "warn", "sync-failed", run.Reason+": "+run.Error
	case run.Status == "ok" && r.NeedsAttention:
		level, code, msg = "info", "recovered", "sync succeeded again"
	}
	if code == "" {
		return
	}
	s.Log.Log(ctx, levelOf(level), "scheduler: "+code, "repo", r.Path, "detail", msg)
	_ = s.Store.AddEvent(ctx, store.Event{Time: s.Clock.Now(), Level: level, RepoID: r.ID, Code: code, Message: msg})
}

func levelOf(l string) slog.Level {
	switch l {
	case "error":
		return slog.LevelError
	case "warn":
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// Wake asks the scheduler to look for due work now instead of at the next tick.
func (s *Scheduler) Wake() {
	s.init()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
