// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/obs"
	"github.com/basmulder03/repo-keeper/internal/paths"
	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/store"
	"github.com/basmulder03/repo-keeper/internal/ui"
)

const (
	cmdLogSize     = 300
	configHistory  = 10
	bundleEvents   = 200
	bundleAuditMax = 200
)

// cmdLog is a bounded ring of recent git invocations for the debug page.
type cmdLog struct {
	mu   sync.Mutex
	recs []gitx.CommandRecord
}

func (c *cmdLog) add(r gitx.CommandRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recs) >= cmdLogSize {
		copy(c.recs, c.recs[1:])
		c.recs = c.recs[:cmdLogSize-1]
	}
	c.recs = append(c.recs, r)
}

func (c *cmdLog) snapshot() []gitx.CommandRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]gitx.CommandRecord(nil), c.recs...)
}

// startUI launches the web interface unless disabled; failures are logged, never fatal to syncing.
func (d *Daemon) startUI(ctx context.Context, cfg config.Config) {
	if d.NoUI || !cfg.UIEnabled() {
		return
	}
	port := cfg.UIPort()
	if d.EphemeralUI {
		port = 0
	}
	l, err := ui.Listen(port)
	if err != nil {
		d.Log.Error("web UI could not start; syncing continues", "err", err)
		return
	}
	d.uiAddr.Store(l.Addr().String())
	dir := d.RuntimeDir
	if dir == "" {
		if dir, err = paths.RuntimeDir(); err != nil {
			dir = d.StateDir
		}
	}
	srv := &ui.Server{Backend: uiBackend{d}, Log: d.Log, Clock: d.Clock, Version: d.version()}
	go func() {
		if err := srv.Serve(ctx, l, filepath.Join(dir, "ui.json")); err != nil {
			d.Log.Error("web UI stopped", "err", err)
		}
	}()
}

type uiBackend struct{ d *Daemon }

func (b uiBackend) Info() ui.Info {
	addr, _ := b.d.uiAddr.Load().(string)
	return ui.Info{
		Version: b.d.version(), GitVersion: b.d.gitVersion, GoVersion: runtime.Version(),
		ConfigPath: b.d.ConfigPath, StateDir: b.d.StateDir, UIAddr: addr, Started: b.d.started,
	}
}

func (b uiBackend) Repos(ctx context.Context) ([]store.Repo, error) { return b.d.Store.ListRepos(ctx) }
func (b uiBackend) Repo(ctx context.Context, id int64) (store.Repo, error) {
	return b.d.Store.Get(ctx, id)
}
func (b uiBackend) Runs(ctx context.Context, id int64, n int) ([]store.Run, error) {
	return b.d.Store.RecentRuns(ctx, id, n)
}
func (b uiBackend) Accounts(ctx context.Context) ([]store.Account, error) {
	return b.d.Store.ListAccounts(ctx)
}
func (b uiBackend) Events(ctx context.Context, n int) ([]store.Event, error) {
	return b.d.Store.RecentEvents(ctx, n)
}
func (b uiBackend) Hosts() []ratelimit.HostState   { return b.d.Limiter.Snapshot() }
func (b uiBackend) Commands() []gitx.CommandRecord { return b.d.cmds.snapshot() }

func (b uiBackend) Cleanups(ctx context.Context) ([]ui.CleanupView, error) {
	repos, err := b.d.Store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := b.d.Store.LatestRuns(ctx)
	if err != nil {
		return nil, err
	}
	l := b.d.live.Load()
	var out []ui.CleanupView
	for _, r := range repos {
		run, ok := latest[r.ID]
		if !ok || run.Detail == "" {
			continue
		}
		var rep cleanup.Report
		if json.Unmarshal([]byte(run.Detail), &rep) != nil {
			continue
		}
		mode := string(rep.Mode)
		if tg, ok := b.d.targetFor(l, r); ok {
			mode = string(tg.set.Policy.Mode)
		}
		out = append(out, ui.CleanupView{Repo: r, Mode: mode, Report: rep})
	}
	return out, nil
}

// Audit returns up to n newest journal entries.
func (b uiBackend) Audit(_ context.Context, n int) ([]audit.Entry, error) {
	all, err := b.d.journal.ReadAll()
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if len(all) > n {
		all = all[:n]
	}
	return all, nil
}

func (b uiBackend) gitFor(ctx context.Context, id int64) (store.Repo, *gitx.Repo, error) {
	r, err := b.d.Store.Get(ctx, id)
	if err != nil {
		return r, nil, err
	}
	return r, b.d.runner.Repo(r.Path), nil
}

func (b uiBackend) Trash(ctx context.Context, id int64) ([]gitx.TrashRef, error) {
	_, g, err := b.gitFor(ctx, id)
	if err != nil {
		return nil, err
	}
	return g.TrashList(ctx)
}

// Status summarises the fleet for the tray helper.
func (b uiBackend) Status(ctx context.Context) (ui.Status, error) {
	st := ui.Status{Version: b.d.version(), Paused: b.d.Sched.Paused()}
	repos, err := b.d.Store.ListRepos(ctx)
	if err != nil {
		return st, err
	}
	st.Repos = len(repos)
	for _, r := range repos {
		switch {
		case r.NeedsAttention:
			st.NeedAttention++
		case r.LastStatus == "":
			st.Pending++
		case r.LastStatus == "ok":
			st.UpToDate++
		default:
			st.NeedAttention++ // failed but not yet flagged still deserves a glance
		}
	}
	accts, err := b.d.Store.ListAccounts(ctx)
	if err != nil {
		return st, err
	}
	st.Accounts = len(accts)
	for _, a := range accts {
		if a.Status != "" && a.Status != "ok" {
			st.AccountsAttention++
		}
	}
	return st, nil
}

// SyncAll queues every active repository now; the rate limiter still paces the actual work.
func (b uiBackend) SyncAll(ctx context.Context) error {
	repos, err := b.d.Store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if err := b.d.Sched.TriggerNow(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// SetPaused pauses or resumes scheduled syncs.
func (b uiBackend) SetPaused(p bool) { b.d.Sched.SetPaused(p) }

func (b uiBackend) SyncNow(ctx context.Context, id int64) error {
	if _, err := b.d.Store.Get(ctx, id); err != nil {
		return err
	}
	return b.d.Sched.TriggerNow(ctx, id)
}

func (b uiBackend) DiscoverNow(_ context.Context, account string) error {
	if _, ok := b.d.live.Load().accounts[account]; !ok {
		return fmt.Errorf("unknown account %q", account)
	}
	b.d.discMu.Lock()
	delete(b.d.nextDisc, account)
	b.d.discMu.Unlock()
	select {
	case b.d.discWake <- struct{}{}:
	default:
	}
	return nil
}

// CleanupNow applies the safe-deletion rules immediately, whatever the configured mode; dirty trees still block it.
func (b uiBackend) CleanupNow(ctx context.Context, id int64) (cleanup.Report, error) {
	repo, g, err := b.gitFor(ctx, id)
	if err != nil {
		return cleanup.Report{}, err
	}
	tg, ok := b.d.targetFor(b.d.live.Load(), repo)
	if !ok {
		return cleanup.Report{}, errors.New("repository is no longer in the configuration")
	}
	if repo.DefaultBranch == "" {
		return cleanup.Report{}, errors.New("default branch not known yet; sync the repository first")
	}
	unlock, err := g.Lock(ctx)
	if err != nil {
		return cleanup.Report{}, err
	}
	defer unlock()
	if err := g.CheckSafeConfig(ctx); err != nil {
		return cleanup.Report{}, err
	}
	var lookup cleanup.MergedLookup
	if tg.acct != nil {
		if prov, _, err := tg.acct.get(); err == nil {
			lookup = func(ctx context.Context, names []string) (map[string]string, error) {
				return prov.MergedBranches(ctx, provider.Repo{FullName: repo.FullName}, names)
			}
		}
	}
	pol := tg.set.Policy
	pol.Mode = cleanup.ModeAuto
	rep, err := cleanup.Run(ctx, cleanup.Input{
		Git: g, Repo: repo.Path, Remote: tg.set.Remote, DefaultBranch: repo.DefaultBranch,
		Policy: pol, Journal: b.d.journal, Clock: b.d.Clock, ProviderMerged: lookup,
	})
	if err == nil {
		d := b.d
		d.event(ctx, "info", repo.ID, "cleanup-manual", fmt.Sprintf("%s: %d item(s) evaluated", repo.Path, len(rep.Items)))
	}
	return rep, err
}

func (b uiBackend) Restore(ctx context.Context, id int64, branch string) error {
	repo, g, err := b.gitFor(ctx, id)
	if err != nil {
		return err
	}
	_, err = cleanup.Restore(ctx, g, b.d.journal, repo.Path, branch, b.d.Clock.Now())
	return err
}

func versionOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// ConfigReadOnly reports a declaratively managed config (a symlink, e.g. into /nix/store): replacing it would fight the tool that owns it.
func (b uiBackend) ConfigReadOnly() string {
	if fi, err := os.Lstat(b.d.ConfigPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(b.d.ConfigPath)
		return "this file is a symlink to " + target + " and is managed outside repo-keeper (for example by Home Manager); change it there"
	}
	return ""
}

func (b uiBackend) Config() (string, string, error) {
	data, err := os.ReadFile(b.d.ConfigPath)
	if err != nil {
		return "", "", err
	}
	return string(data), versionOf(string(data)), nil
}

// SaveConfig validates, keeps the previous file in the history, replaces atomically and asks the daemon to reload.
func (b uiBackend) SaveConfig(_ context.Context, text, version string) error {
	if why := b.ConfigReadOnly(); why != "" {
		return errors.New("configuration is read-only: " + why)
	}
	cur, err := os.ReadFile(b.d.ConfigPath)
	if err != nil {
		return err
	}
	if version != "" && version != versionOf(string(cur)) {
		return errors.New("the configuration changed on disk since you opened it; reload this page and re-apply your edit")
	}
	if _, err := config.Parse([]byte(text)); err != nil {
		return err
	}
	if err := b.d.archiveConfig(cur); err != nil {
		return fmt.Errorf("could not keep a backup of the current configuration: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(b.d.ConfigPath), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), b.d.ConfigPath); err != nil {
		return err
	}
	select {
	case b.d.reloadWake <- struct{}{}:
	default:
	}
	return nil
}

// archiveConfig stores old under <state>/config-history and keeps the newest configHistory files.
func (d *Daemon) archiveConfig(old []byte) error {
	dir := filepath.Join(d.StateDir, "config-history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("config-%s.toml", d.Clock.Now().UTC().Format("20060102T150405.000"))
	// #nosec G703 -- dir is the daemon state dir and name is generated from the clock
	if err := os.WriteFile(filepath.Join(dir, name), old, 0o600); err != nil { //nolint:gosec // see #nosec above
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for len(ents) > configHistory {
		_ = os.Remove(filepath.Join(dir, ents[0].Name()))
		ents = ents[1:]
	}
	return nil
}

// Bundle assembles the diagnostics download; everything passes through the redactor.
func (b uiBackend) Bundle(ctx context.Context) ([]byte, error) {
	d := b.d
	repos, _ := d.Store.ListRepos(ctx)
	accounts, _ := d.Store.ListAccounts(ctx)
	events, _ := d.Store.RecentEvents(ctx, bundleEvents)
	aud, _ := b.Audit(ctx, bundleAuditMax)
	text, _, _ := b.Config()
	type hostView struct {
		Host       string
		Cooldown   time.Time
		CooldownBy string
		Failures   int
		Open       bool
		Quota      ratelimit.Quota
	}
	var hosts []hostView
	for _, h := range d.Limiter.Snapshot() {
		hosts = append(hosts, hostView{h.Host, h.Cooldown, h.CooldownBy, h.Failures, h.CircuitOpen, h.Quota})
	}
	bundle := map[string]any{
		"generated": d.Clock.Now(), "info": b.Info(), "config": d.Redactor.Scrub(obs.Scrub(text)),
		"repos": repos, "accounts": accounts, "hosts": hosts, "events": events, "commands": d.cmds.snapshot(), "audit": aud,
	}
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, err
	}
	return []byte(d.Redactor.Scrub(obs.Scrub(string(raw)))), nil
}
