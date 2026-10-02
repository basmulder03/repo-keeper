// SPDX-License-Identifier: Apache-2.0

// Package daemon wires config, store, rate limiting, scheduling and the sync engine into the background service.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/instance"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/sched"
	"github.com/basmulder03/repo-keeper/internal/store"
	"github.com/basmulder03/repo-keeper/internal/syncer"
)

const (
	reloadEvery    = 30 * time.Second
	maxLimiterWait = 30 * time.Second
)

// Daemon is the long-running service.
type Daemon struct {
	ConfigPath string
	StateDir   string
	Runner     *gitx.Runner
	Clock      clock.Clock
	Log        *slog.Logger
	// Tick overrides the scheduler poll interval (tests).
	Tick time.Duration

	live      atomic.Pointer[liveConfig]
	ready     chan struct{}
	readyOnce sync.Once
	Store     *store.Store
	Limiter   *ratelimit.Limiter
	Sched     *sched.Scheduler
	journal   *audit.File
}

// liveConfig is an immutable snapshot swapped atomically on reload.
type liveConfig struct {
	cfg    config.Config
	quiet  config.QuietHours
	byPath map[string]config.RepoSettings
}

func newLive(c config.Config) *liveConfig {
	l := &liveConfig{cfg: c, byPath: map[string]config.RepoSettings{}}
	l.quiet, _ = config.ParseQuietHours(c.General.QuietHours) // validated already; zero value = never quiet
	for _, r := range c.Repos {
		s := c.Resolve(r)
		l.byPath[s.Path] = s
	}
	return l
}

// StatePaths returns the database, lock and journal locations under dir.
func StatePaths(dir string) (db, lock, journal string) {
	return filepath.Join(dir, "state.db"), filepath.Join(dir, "daemon.lock"), filepath.Join(dir, "audit.jsonl")
}

// Ready is closed once Store, Limiter and Sched are usable (nil-safe to wait on before Run).
func (d *Daemon) Ready() <-chan struct{} {
	d.initReady()
	return d.ready
}

func (d *Daemon) initReady() {
	d.readyOnce.Do(func() { d.ready = make(chan struct{}) })
}

// Run starts the daemon and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.initReady()
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	dbPath, lockPath, journalPath := StatePaths(d.StateDir)

	lock, err := instance.Acquire(lockPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d.Store = st
	if d.journal, err = audit.OpenFile(journalPath); err != nil {
		return err
	}

	cfg, err := config.Load(d.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no configuration at %s (run `repo-keeper init` first)", d.ConfigPath)
		}
		return err
	}
	l := newLive(cfg)
	d.live.Store(l)
	d.Limiter = ratelimit.New(ratelimit.Config{MaxConcurrent: cfg.General.PerHost}, d.Clock, rand.Float64) //nolint:gosec // #nosec G404 -- jitter, not security
	if err := d.reconcile(ctx, l); err != nil {
		return err
	}

	d.Sched = &sched.Scheduler{
		Store: st, Job: d.job, Clock: d.Clock, Log: d.Log, Tick: d.Tick, Workers: cfg.General.Concurrency,
		Quiet: func(t time.Time) bool { return d.live.Load().quiet.Contains(t) },
	}
	close(d.ready)
	d.Log.Info("daemon started", "repos", len(cfg.Repos), "config", d.ConfigPath, "state", d.StateDir)
	go d.reloadLoop(ctx, cfg)
	err = d.Sched.Run(ctx)
	d.Log.Info("daemon stopped")
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// reconcile makes the store's active repo set match the config; new repos are staggered over a few minutes.
func (d *Daemon) reconcile(ctx context.Context, l *liveConfig) error {
	now := d.Clock.Now()
	specs := make([]store.Spec, 0, len(l.cfg.Repos))
	for _, r := range l.cfg.Repos {
		s := l.cfg.Resolve(r)
		specs = append(specs, store.Spec{Path: s.Path, Remote: s.Remote, Interval: s.Interval})
	}
	return d.Store.SyncRepos(ctx, specs, func(i int) time.Time { return now.Add(time.Duration(i) * 2 * time.Second) })
}

// reloadLoop polls the config file and applies valid changes; invalid files never replace a working config.
func (d *Daemon) reloadLoop(ctx context.Context, initial config.Config) {
	var lastStamp string
	if st, err := os.Stat(d.ConfigPath); err == nil {
		lastStamp = stamp(st)
	}
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.Clock.After(reloadEvery):
		}
		st, err := os.Stat(d.ConfigPath)
		if err != nil || stamp(st) == lastStamp {
			continue
		}
		lastStamp = stamp(st)
		cfg, err := config.Load(d.ConfigPath)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				d.Log.Error("config reload rejected; keeping previous configuration", "err", err)
				_ = d.Store.AddEvent(ctx, store.Event{Time: d.Clock.Now(), Level: "error", Code: "config-invalid", Message: msg})
			}
			continue
		}
		lastErr = ""
		l := newLive(cfg)
		d.live.Store(l)
		if err := d.reconcile(ctx, l); err != nil {
			d.Log.Error("config reload: reconcile failed", "err", err)
			continue
		}
		if cfg.General.Concurrency != initial.General.Concurrency || cfg.General.PerHost != initial.General.PerHost {
			d.Log.Warn("concurrency/per_host changes apply after restart")
		}
		d.Sched.Wake()
		d.Log.Info("config reloaded", "repos", len(cfg.Repos))
		_ = d.Store.AddEvent(ctx, store.Event{Time: d.Clock.Now(), Level: "info", Code: "config-reloaded", Message: fmt.Sprintf("%d repos", len(cfg.Repos))})
	}
}

func stamp(fi os.FileInfo) string { return fmt.Sprintf("%d-%d", fi.ModTime().UnixNano(), fi.Size()) }

// job syncs one repository under the host's rate limit.
func (d *Daemon) job(ctx context.Context, repo store.Repo) sched.Result {
	l := d.live.Load()
	set, ok := l.byPath[repo.Path]
	if !ok { // removed from config between dispatch and run
		return sched.Result{NoRecord: true, Kind: sched.Success}
	}
	g := d.Runner.Repo(repo.Path)
	host := repo.Host
	if host == "" {
		if u, err := g.RemoteURL(ctx, set.Remote); err == nil {
			host = hostOf(u)
			_ = d.Store.SetHost(ctx, repo.ID, host)
		} else {
			host = "local"
		}
	}

	permit, err := d.Limiter.Acquire(ctx, host, maxLimiterWait)
	if err != nil {
		var we *ratelimit.WaitError
		if errors.As(err, &we) {
			return sched.Result{NoRecord: true, Kind: sched.RateLimited, RetryAt: we.RetryAt}
		}
		return sched.Result{NoRecord: true, Kind: sched.Transient} // context cancelled
	}

	started := d.Clock.Now()
	res := syncer.Sync(ctx, syncer.Input{
		Git: g, Path: repo.Path, Remote: set.Remote, AllBranches: set.AllBranches, PrevDigest: repo.Digest,
		Cleanup: set.Policy, Journal: d.journal, Clock: d.Clock,
	})
	v := classify(res)
	permit.Release(v.resp)

	out := sched.Result{Kind: v.kind, Run: store.Run{
		Started: started, Finished: d.Clock.Now(), Status: string(res.Status), Reason: res.Reason, FF: string(res.FF),
		Fetched: res.Fetched, DefaultBranch: res.DefaultBranch, Digest: res.Digest,
	}}
	if res.Err != nil {
		out.Run.Error = res.Err.Error()
	}
	if res.Cleanup != nil {
		if b, err := json.Marshal(res.Cleanup); err == nil {
			out.Run.Detail = string(b)
		}
	}
	if v.kind == sched.RateLimited {
		out.RetryAt = retryAtFrom(d.Limiter.CooldownUntil(host), d.Clock.Now())
	}
	return out
}
