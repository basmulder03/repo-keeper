// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/provider"
	_ "github.com/basmulder03/repo-keeper/internal/provider/github" // registers the github provider
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/secrets"
	"github.com/basmulder03/repo-keeper/internal/store"
)

const (
	tokenCacheTTL    = 5 * time.Minute
	credentialRetry  = 15 * time.Minute
	authFailedRetry  = time.Hour
	discoveryReserve = 0.10 // stop listing when under 10 % of the API quota remains
)

// accountRT is one configured account at runtime; its token is re-read periodically so rotation just works.
type accountRT struct {
	set config.AccountSettings
	d   *Daemon

	mu    sync.Mutex
	prov  provider.Provider
	token secrets.Token
	at    time.Time
}

func (a *accountRT) source() secrets.Source {
	return secrets.Source{Env: a.set.TokenEnv, File: a.set.TokenFile, Key: "account/" + a.set.Name}
}

// get returns a provider with a fresh-enough token.
func (a *accountRT) get() (provider.Provider, secrets.Token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.d.Clock.Now()
	if a.prov != nil && now.Sub(a.at) < tokenCacheTTL {
		return a.prov, a.token, nil
	}
	tok, err := a.source().Resolve(a.d.Secrets)
	if err != nil {
		return nil, secrets.Token{}, err
	}
	a.at = now
	if a.prov == nil || tok.Reveal() != a.token.Reveal() {
		a.d.Redactor.Add(tok.Reveal())
		p, err := provider.New(provider.Kind(a.set.Provider), provider.Config{BaseURL: a.set.BaseURL, Token: tok, HTTP: a.d.HTTP})
		if err != nil {
			return nil, secrets.Token{}, err
		}
		a.prov, a.token = p, tok
	}
	return a.prov, a.token, nil
}

// invalidate forces the next get to re-read the credential (after an auth failure).
func (a *accountRT) invalidate() {
	a.mu.Lock()
	a.at = time.Time{}
	a.mu.Unlock()
}

func (d *Daemon) buildAccounts(cfg config.Config) map[string]*accountRT {
	out := map[string]*accountRT{}
	for _, a := range cfg.Accounts {
		out[a.Name] = &accountRT{set: cfg.ResolveAccount(a), d: d}
	}
	return out
}

// discoveryLoop lists each account's repositories on its own (slow) cadence.
func (d *Daemon) discoveryLoop(ctx context.Context) {
	for {
		d.discoverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.Clock.After(30 * time.Second):
		}
	}
}

func (d *Daemon) discoverDue(ctx context.Context) {
	l := d.live.Load()
	names := make([]string, 0, len(l.accounts))
	for n := range l.accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	now := d.Clock.Now()
	for _, n := range names {
		d.discMu.Lock()
		next, known := d.nextDisc[n]
		d.discMu.Unlock()
		if known && now.Before(next) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		d.discover(ctx, l, l.accounts[n])
	}
}

func (d *Daemon) setNext(name string, t time.Time) {
	d.discMu.Lock()
	d.nextDisc[name] = t
	d.discMu.Unlock()
}

// discover refreshes one account: auth check, listing, filtering, reconciliation.
func (d *Daemon) discover(ctx context.Context, l *liveConfig, a *accountRT) {
	now := d.Clock.Now()
	name := a.set.Name
	prev := d.accountState(name)
	st := store.Account{Name: name, Provider: a.set.Provider, Checked: now, Login: prev.Login, Discovered: prev.Discovered, RepoCount: prev.RepoCount}
	finish := func(status, errMsg string, next time.Time) {
		st.Status, st.Error, st.NextDiscovery = status, errMsg, next
		d.setNext(name, next)
		if err := d.Store.SaveAccount(ctx, st); err != nil {
			d.Log.Error("saving account state", "account", name, "err", err)
		}
		d.setAccountState(st)
		if status != prev.Status && status != "ok" {
			d.event(ctx, "error", 0, "account-"+status, name+": "+errMsg)
		} else if status == "ok" && prev.Status != "" && prev.Status != "ok" {
			d.event(ctx, "info", 0, "account-recovered", name)
		}
	}

	prov, _, err := a.get()
	if err != nil {
		finish("no-credential", err.Error(), now.Add(credentialRetry))
		return
	}
	auth, err := prov.CheckAuth(ctx)
	if err != nil {
		var we *ratelimit.WaitError
		switch {
		case errors.As(err, &we):
			d.setNext(name, we.RetryAt)
		case errors.Is(err, provider.ErrAuth):
			a.invalidate()
			finish("auth-failed", err.Error(), now.Add(authFailedRetry))
		case ctx.Err() != nil:
		default:
			finish("error", err.Error(), now.Add(credentialRetry))
		}
		return
	}
	st.Login, st.Expires, st.Warnings = auth.Login, auth.Expires, strings.Join(auth.Warnings, "; ")
	if st.Warnings != prev.Warnings && st.Warnings != "" {
		d.event(ctx, "warn", 0, "account-warning", name+": "+st.Warnings)
	}

	if !d.Limiter.HasReserve(prov.APIHost(), discoveryReserve) {
		q := d.Limiter.Budget(prov.APIHost())
		finish("ok", "API quota low; listing postponed until reset", q.Reset)
		return
	}
	repos, err := prov.ListRepos(ctx)
	if err != nil {
		var we *ratelimit.WaitError
		switch {
		case errors.As(err, &we):
			d.setNext(name, we.RetryAt)
		case errors.Is(err, provider.ErrAuth):
			a.invalidate()
			finish("auth-failed", err.Error(), now.Add(authFailedRetry))
		case ctx.Err() != nil:
		default:
			finish("error", err.Error(), now.Add(credentialRetry))
		}
		return
	}

	specs, skipped := d.specsFor(l, a, repos)
	for _, s := range skipped {
		d.event(ctx, "warn", 0, "repo-skipped", name+": "+s)
	}
	res, err := d.Store.SyncManaged(ctx, name, specs, func(i int) time.Time { return now.Add(time.Duration(i) * 2 * time.Second) })
	if err != nil {
		finish("error", err.Error(), now.Add(credentialRetry))
		return
	}
	for _, p := range res.Conflicts {
		d.event(ctx, "warn", 0, "path-conflict", fmt.Sprintf("%s: %s is already managed elsewhere", name, p))
	}
	if res.Added > 0 || res.Missing > 0 {
		d.event(ctx, "info", 0, "discovery", fmt.Sprintf("%s: %d new, %d no longer listed, %d tracked", name, res.Added, res.Missing, len(specs)))
	}
	st.Discovered, st.RepoCount = now, len(specs)
	// #nosec G404 -- scheduling jitter, not security
	jitter := time.Duration((rand.Float64()*0.2 - 0.1) * float64(a.set.Discovery)) //nolint:gosec // see #nosec above
	finish("ok", "", now.Add(a.set.Discovery+jitter))
	d.Sched.Wake()
}

// specsFor applies include/exclude and platform rules and maps repositories to local paths.
func (d *Daemon) specsFor(l *liveConfig, a *accountRT, repos []provider.Repo) (specs []store.Spec, skipped []string) {
	kind := provider.Kind(a.set.Provider)
	for _, r := range repos {
		switch {
		case r.Disabled, a.set.SkipArchivedRepos && r.Archived, a.set.SkipForks && r.Fork:
			continue
		case !provider.Matches(a.set.Include, a.set.Exclude, r):
			continue
		}
		p, err := provider.LocalPath(l.cfg.General.Root, kind, r)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		url := r.CloneURL
		if a.set.UseSSH {
			url = r.SSHURL
		}
		if url == "" {
			skipped = append(skipped, r.FullName+": platform returned no clone URL")
			continue
		}
		specs = append(specs, store.Spec{
			Path: p, Remote: "origin", Interval: a.set.SyncInterval,
			FullName: r.FullName, RemoteID: r.ID, CloneURL: url, Archived: r.Archived,
		})
	}
	return specs, skipped
}

func (d *Daemon) event(ctx context.Context, level string, repoID int64, code, msg string) {
	d.Log.Log(ctx, levelOf(level), code, "detail", msg)
	_ = d.Store.AddEvent(ctx, store.Event{Time: d.Clock.Now(), Level: level, RepoID: repoID, Code: code, Message: msg})
}

func (d *Daemon) accountState(name string) store.Account {
	d.discMu.Lock()
	defer d.discMu.Unlock()
	return d.accounts[name]
}

func (d *Daemon) setAccountState(a store.Account) {
	d.discMu.Lock()
	d.accounts[a.Name] = a
	d.discMu.Unlock()
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
