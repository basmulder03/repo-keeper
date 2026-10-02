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
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/instance"
	"github.com/basmulder03/repo-keeper/internal/obs"
	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/sched"
	"github.com/basmulder03/repo-keeper/internal/secrets"
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
	// NoUI disables the web interface; EphemeralUI binds any free port instead of the configured one (tests).
	NoUI        bool
	EphemeralUI bool
	// RuntimeDir receives ui.json (default: per-user runtime dir).
	RuntimeDir string
	// Version is reported in the User-Agent.
	Version string
	// Secrets resolves account tokens (default: OS keychain).
	Secrets secrets.Store
	// Redactor, when set, learns every token so logs can never contain one.
	Redactor *obs.Redactor

	live      atomic.Pointer[liveConfig]
	ready     chan struct{}
	readyOnce sync.Once
	Store     *store.Store
	Limiter   *ratelimit.Limiter
	HTTP      *httpx.Client
	Sched     *sched.Scheduler
	journal   *audit.File

	runner     *gitx.Runner
	cmds       cmdLog
	started    time.Time
	gitVersion string
	uiAddr     atomic.Value
	reloadWake chan struct{}
	discWake   chan struct{}
	discMu     sync.Mutex
	nextDisc   map[string]time.Time
	accounts   map[string]store.Account
}

// liveConfig is an immutable snapshot swapped atomically on reload.
type liveConfig struct {
	cfg      config.Config
	quiet    config.QuietHours
	byPath   map[string]config.RepoSettings
	accounts map[string]*accountRT
}

func (d *Daemon) newLive(c config.Config) *liveConfig {
	l := &liveConfig{cfg: c, byPath: map[string]config.RepoSettings{}, accounts: d.buildAccounts(c)}
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

func (d *Daemon) version() string {
	if d.Version == "" {
		return "dev"
	}
	return d.Version
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
	if d.Secrets == nil {
		d.Secrets = secrets.Keyring{}
	}
	if d.Redactor == nil {
		d.Redactor = &obs.Redactor{}
	}
	d.nextDisc, d.accounts = map[string]time.Time{}, map[string]store.Account{}
	d.reloadWake, d.discWake = make(chan struct{}, 1), make(chan struct{}, 1)
	d.started = d.Clock.Now()
	r := *d.Runner // private copy so only the daemon records commands
	r.Observe = d.cmds.add
	d.runner = &r
	if v, err := r.Version(ctx); err == nil {
		d.gitVersion = v.String()
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
	d.Limiter = ratelimit.New(ratelimit.Config{MaxConcurrent: cfg.General.PerHost}, d.Clock, rand.Float64) //nolint:gosec // #nosec G404 -- jitter, not security
	if d.HTTP, err = httpx.New(httpx.Config{
		UserAgent: "repo-keeper/" + d.version() + " (+https://github.com/basmulder03/repo-keeper)",
		Limiter:   d.Limiter, Clock: d.Clock, MaxWait: maxLimiterWait,
	}); err != nil {
		return err
	}
	l := d.newLive(cfg)
	d.live.Store(l)
	if err := d.reconcile(ctx, l); err != nil {
		return err
	}
	d.loadAccountState(ctx)

	d.Sched = &sched.Scheduler{
		Store: st, Job: d.job, Clock: d.Clock, Log: d.Log, Tick: d.Tick, Workers: cfg.General.Concurrency,
		Quiet: func(t time.Time) bool { return d.live.Load().quiet.Contains(t) },
	}
	close(d.ready)
	d.startUI(ctx, cfg)
	d.Log.Info("daemon started", "repos", len(cfg.Repos), "accounts", len(cfg.Accounts), "config", d.ConfigPath, "state", d.StateDir)
	go d.reloadLoop(ctx, cfg)
	go d.discoveryLoop(ctx)
	err = d.Sched.Run(ctx)
	d.Log.Info("daemon stopped")
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// loadAccountState restores discovery schedules so a restart does not re-list every account immediately.
func (d *Daemon) loadAccountState(ctx context.Context) {
	as, err := d.Store.ListAccounts(ctx)
	if err != nil {
		return
	}
	for _, a := range as {
		d.accounts[a.Name] = a
		if !a.NextDiscovery.IsZero() {
			d.nextDisc[a.Name] = a.NextDiscovery
		}
	}
}

// reconcile makes the store's active config-listed repo set match the config; new repos are staggered.
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
		case <-d.reloadWake:
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
		l := d.newLive(cfg)
		d.live.Store(l)
		d.discMu.Lock()
		for name := range d.nextDisc { // changed account settings take effect at once; removed accounts are forgotten
			delete(d.nextDisc, name)
		}
		d.discMu.Unlock()
		if err := d.reconcile(ctx, l); err != nil {
			d.Log.Error("config reload: reconcile failed", "err", err)
			continue
		}
		if cfg.General.Concurrency != initial.General.Concurrency || cfg.General.PerHost != initial.General.PerHost {
			d.Log.Warn("concurrency/per_host changes apply after restart")
		}
		d.Sched.Wake()
		d.Log.Info("config reloaded", "repos", len(cfg.Repos), "accounts", len(cfg.Accounts))
		_ = d.Store.AddEvent(ctx, store.Event{Time: d.Clock.Now(), Level: "info", Code: "config-reloaded", Message: fmt.Sprintf("%d repos, %d accounts", len(cfg.Repos), len(cfg.Accounts))})
	}
}

func stamp(fi os.FileInfo) string { return fmt.Sprintf("%d-%d", fi.ModTime().UnixNano(), fi.Size()) }

// target is everything a job needs to know about where a repo comes from.
type target struct {
	set  config.RepoSettings
	acct *accountRT // nil for repos listed directly in the config
}

func (d *Daemon) targetFor(l *liveConfig, repo store.Repo) (target, bool) {
	if repo.Source == "" {
		s, ok := l.byPath[repo.Path]
		return target{set: s}, ok
	}
	a, ok := l.accounts[repo.Source]
	if !ok {
		return target{}, false
	}
	return target{acct: a, set: config.RepoSettings{
		Path: repo.Path, Remote: "origin", Interval: a.set.SyncInterval, Policy: a.set.Policy,
		AllBranches: l.cfg.General.AllBranches == nil || *l.cfg.General.AllBranches,
	}}, true
}

func isGitRepo(path string) bool {
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}

// credFor builds an HTTPS credential scoped to the remote's host; SSH and local remotes need none.
func credFor(rawURL, user string, tok secrets.Token) *gitx.Cred {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || tok.IsZero() {
		return nil
	}
	return &gitx.Cred{Host: u.Host, Username: user, Secret: tok}
}

func failure(reason string, err error, k sched.Kind, started, now time.Time) sched.Result {
	return sched.Result{Kind: k, Run: store.Run{Started: started, Finished: now, Status: "failed", Reason: reason, Error: err.Error()}}
}

// job syncs (and for discovered repos first clones) one repository under the host's rate limit.
func (d *Daemon) job(ctx context.Context, repo store.Repo) sched.Result {
	l := d.live.Load()
	tg, ok := d.targetFor(l, repo)
	if !ok { // removed from config between dispatch and run
		return sched.Result{NoRecord: true, Kind: sched.Success}
	}
	started := d.Clock.Now()
	var tok secrets.Token
	var prov provider.Provider
	if tg.acct != nil {
		var err error
		if prov, tok, err = tg.acct.get(); err != nil {
			return failure("no-credential", err, sched.NeedsUser, started, d.Clock.Now())
		}
	}

	if tg.acct != nil && !isGitRepo(repo.Path) {
		return d.clone(ctx, repo, tg, tok, prov.GitUsername(), started)
	}

	g := d.runner.Repo(repo.Path)
	remoteURL, _ := g.RemoteURL(ctx, tg.set.Remote)
	host := repo.Host
	if h := hostOf(remoteURL); remoteURL != "" && h != host {
		host = h
		_ = d.Store.SetHost(ctx, repo.ID, host)
	}
	if host == "" {
		host = "local"
	}
	var lookup cleanup.MergedLookup
	if tg.acct != nil {
		g = g.WithCred(credFor(remoteURL, prov.GitUsername(), tok))
		lookup = func(ctx context.Context, names []string) (map[string]string, error) {
			return prov.MergedBranches(ctx, provider.Repo{FullName: repo.FullName}, names)
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

	res := syncer.Sync(ctx, syncer.Input{
		Git: g, Path: repo.Path, Remote: tg.set.Remote, AllBranches: tg.set.AllBranches, PrevDigest: repo.Digest,
		Cleanup: tg.set.Policy, Journal: d.journal, Clock: d.Clock, ProviderMerged: lookup,
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

// clone fetches a newly discovered repository; an occupied path is never overwritten.
func (d *Daemon) clone(ctx context.Context, repo store.Repo, tg target, tok secrets.Token, gitUser string, started time.Time) sched.Result {
	if _, err := os.Lstat(repo.Path); err == nil {
		return failure("path-occupied", fmt.Errorf("%s exists but is not a git repository; move it away or exclude %s", repo.Path, repo.FullName), sched.NeedsUser, started, d.Clock.Now())
	}
	host := hostOf(repo.CloneURL)
	if host != repo.Host {
		_ = d.Store.SetHost(ctx, repo.ID, host)
	}
	permit, err := d.Limiter.Acquire(ctx, host, maxLimiterWait)
	if err != nil {
		var we *ratelimit.WaitError
		if errors.As(err, &we) {
			return sched.Result{NoRecord: true, Kind: sched.RateLimited, RetryAt: we.RetryAt}
		}
		return sched.Result{NoRecord: true, Kind: sched.Transient}
	}
	err = d.runner.Clone(ctx, repo.CloneURL, repo.Path, credFor(repo.CloneURL, gitUser, tok), tg.acct.set.PartialClone)
	res := syncer.Result{Status: syncer.OK}
	if err != nil {
		res = syncer.Result{Status: syncer.Failed, Reason: "clone", Err: err}
	}
	v := classify(res)
	permit.Release(v.resp)

	run := store.Run{Started: started, Finished: d.Clock.Now(), Status: string(res.Status), Reason: res.Reason, Fetched: err == nil}
	if err != nil {
		run.Error = err.Error()
		out := sched.Result{Kind: v.kind, Run: run}
		if v.kind == sched.RateLimited {
			out.RetryAt = retryAtFrom(d.Limiter.CooldownUntil(host), d.Clock.Now())
		}
		return out
	}
	run.Reason = "cloned"
	if def, _ := d.runner.Repo(repo.Path).OriginHead(ctx, "origin"); def != "" {
		run.DefaultBranch = def
	}
	d.event(ctx, "info", repo.ID, "cloned", repo.FullName)
	return sched.Result{Kind: sched.Success, Run: run}
}
