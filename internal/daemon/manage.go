// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/provider/github"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/secrets"
	"github.com/basmulder03/repo-keeper/internal/ui"
)

const (
	credentialCheckTimeout = 25 * time.Second
	deviceFallbackLifetime = 15 * time.Minute
	deviceKeepFor          = 30 * time.Minute
	maxPendingDevices      = 4
)

// configReadOnly explains why the file cannot be edited (a symlink, e.g. into /nix/store), "" when it can.
func (d *Daemon) configReadOnly() string {
	if fi, err := os.Lstat(d.ConfigPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(d.ConfigPath)
		return "the configuration is a symlink to " + target + " and is managed outside repo-keeper (for example by Home Manager); change it there"
	}
	return ""
}

// plan computes an edited config text without writing anything; user mistakes come back as ValidationErrors.
func (d *Daemon) plan(fn func(text string) (string, error)) (cur []byte, next string, err error) {
	if why := d.configReadOnly(); why != "" {
		return nil, "", ui.Invalid("Settings are read-only: " + why)
	}
	cur, err = os.ReadFile(d.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	next, err = fn(string(cur))
	if err != nil {
		return nil, "", asValidation(err)
	}
	return cur, next, nil
}

// asValidation turns config validation text (one problem per line) into something the form can list.
func asValidation(err error) error {
	var ve *ui.ValidationError
	if errors.As(err, &ve) {
		return err
	}
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	if errors.Is(err, config.ErrUnsupportedLayout) {
		lines = []string{"The configuration file uses a layout these forms cannot change safely (for example inline tables). Edit it by hand once, or rewrite it with `[[account]]` / `[[repo]]` tables."}
	}
	return ui.Invalid(lines...)
}

// commit archives the previous file and replaces it atomically, then wakes the reload loop.
func (d *Daemon) commit(cur []byte, next string) error {
	if next == string(cur) {
		return nil
	}
	if err := d.archiveConfig(cur); err != nil {
		return fmt.Errorf("could not keep a backup of the current configuration: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(d.ConfigPath), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(next); err != nil {
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
	if err := os.Rename(tmp.Name(), d.ConfigPath); err != nil {
		return err
	}
	select {
	case d.reloadWake <- struct{}{}:
	default:
	}
	return nil
}

// edit is plan + commit under one lock, for changes that need no credential work.
func (d *Daemon) edit(fn func(text string) (string, error)) error {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	cur, next, err := d.plan(fn)
	if err != nil {
		return err
	}
	return d.commit(cur, next)
}

func (d *Daemon) currentConfig() (config.Config, error) {
	cfg, err := config.Load(d.ConfigPath)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

// ---- settings

func yes(b bool) *bool { return &b }

func lines(s string) []string {
	var out []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (b uiBackend) Settings(_ context.Context) (ui.SettingsView, error) {
	cfg, err := b.d.currentConfig()
	if err != nil {
		return ui.SettingsView{}, err
	}
	f := ui.SettingsForm{
		Root: cfg.General.Root, Interval: config.FormatDuration(time.Duration(cfg.General.Interval)),
		Concurrency: strconv.Itoa(cfg.General.Concurrency), PerHost: strconv.Itoa(cfg.General.PerHost),
		QuietHours: cfg.General.QuietHours, AllBranches: cfg.General.AllBranches == nil || *cfg.General.AllBranches,
		Secrets: orDefault(cfg.General.Secrets, "keyring"), SecretsFile: cfg.General.SecretsFile,
		CleanupMode: cfg.Cleanup.Mode, MinAge: config.FormatDuration(time.Duration(cfg.Cleanup.MinAge)),
		Protected: strings.Join(cfg.Cleanup.Protected, "\n"), AllowNeverPushed: cfg.Cleanup.AllowNeverPushed,
		UIEnabled: cfg.UIEnabled(), UIPort: strconv.Itoa(cfg.UIPort()),
	}
	return ui.SettingsView{Form: f, Repos: cfg.Repos, ReadOnly: b.d.configReadOnly(), Providers: provider.Kinds()}, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (b uiBackend) SaveSettings(_ context.Context, f ui.SettingsForm) (bool, error) {
	var problems []string
	atoi := func(label, v string) int {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			problems = append(problems, label+" must be a whole number")
		}
		return n
	}
	dur := func(label, v string) config.Duration {
		d, err := config.ParseFlexibleDuration(v)
		if err != nil {
			problems = append(problems, label+": "+err.Error())
		}
		return config.Duration(d)
	}
	g := config.General{
		Root: strings.TrimSpace(f.Root), Interval: dur("Sync interval", f.Interval),
		Concurrency: atoi("Parallel syncs", f.Concurrency), PerHost: atoi("Parallel operations per host", f.PerHost),
		QuietHours: strings.TrimSpace(f.QuietHours), AllBranches: yes(f.AllBranches),
		Secrets: f.Secrets, SecretsFile: strings.TrimSpace(f.SecretsFile),
	}
	if g.Secrets == "keyring" {
		g.Secrets = "" // the default is not written out
	}
	c := config.Cleanup{
		Mode: f.CleanupMode, MinAge: dur("Minimum branch age", f.MinAge), Protected: lines(f.Protected),
		AllowNeverPushed: f.AllowNeverPushed,
	}
	if len(c.Protected) == 0 {
		c.Protected = append([]string{}, cleanup.DefaultProtected...) // empty means "the built-in list", written out so it is visible
	}
	u := config.UI{Enabled: yes(f.UIEnabled), Port: atoi("UI port", f.UIPort)}
	if len(problems) > 0 {
		return false, ui.Invalid(problems...)
	}

	old, err := b.d.currentConfig()
	if err != nil {
		return false, err
	}
	restart := old.General.Concurrency != g.Concurrency || old.General.PerHost != g.PerHost ||
		old.UIEnabled() != f.UIEnabled || old.UIPort() != u.Port || old.General.Secrets != g.Secrets || old.General.SecretsFile != g.SecretsFile

	err = b.d.edit(func(text string) (string, error) {
		var err error
		if text, err = config.SetGeneral(text, g); err != nil {
			return "", err
		}
		if text, err = config.SetCleanup(text, c); err != nil {
			return "", err
		}
		return config.SetUI(text, u)
	})
	return restart, err
}

func (b uiBackend) AddRepo(_ context.Context, f ui.RepoForm) error {
	r := config.Repo{Path: strings.TrimSpace(f.Path), Remote: strings.TrimSpace(f.Remote), CleanupMode: f.CleanupMode}
	if f.Interval != "" {
		d, err := config.ParseFlexibleDuration(f.Interval)
		if err != nil {
			return ui.Invalid("Interval: " + err.Error())
		}
		r.Interval = config.Duration(d)
	}
	switch f.AllBranches {
	case "true":
		r.AllBranches = yes(true)
	case "false":
		r.AllBranches = yes(false)
	}
	if r.Path == "" {
		return ui.Invalid("A repository path is required.")
	}
	if fi, err := os.Stat(filepath.Join(r.Path, ".git")); err != nil || fi == nil {
		return ui.Invalid(r.Path + " is not a git repository (no .git inside).")
	}
	return b.d.edit(func(text string) (string, error) { return config.SetRepo(text, r) })
}

func (b uiBackend) RemoveRepo(_ context.Context, path string) error {
	return b.d.edit(func(text string) (string, error) { return config.RemoveRepo(text, path) })
}

// ---- accounts

func (b uiBackend) AccountForm(_ context.Context, name string) (ui.AccountForm, bool, error) {
	cfg, err := b.d.currentConfig()
	if err != nil {
		return ui.AccountForm{}, false, err
	}
	for _, a := range cfg.Accounts {
		if a.Name != name {
			continue
		}
		f := ui.AccountForm{
			Name: a.Name, Provider: a.Provider, BaseURL: a.BaseURL, Auth: "keep", TokenFile: a.TokenFile, TokenEnv: a.TokenEnv,
			ClientID: a.OAuthClientID, WebURL: a.OAuthWebURL, URLs: strings.Join(a.URLs, "\n"), Include: strings.Join(a.Include, "\n"), Exclude: strings.Join(a.Exclude, "\n"),
			CAFile: a.CAFile, CloneProtocol: a.CloneProtocol, SkipArchived: a.SkipArchived == nil || *a.SkipArchived,
			SkipForks: a.SkipForks, PartialClone: a.PartialClone, CleanupMode: a.CleanupMode, Root: cfg.General.Root,
		}
		if a.DiscoveryInterval != 0 {
			f.DiscoveryInterval = config.FormatDuration(time.Duration(a.DiscoveryInterval))
		}
		if a.Interval != 0 {
			f.Interval = config.FormatDuration(time.Duration(a.Interval))
		}
		return f, true, nil
	}
	return ui.AccountForm{}, false, nil
}

// buildAccount converts the form into a config entry. existing (may be nil) supplies what "keep" must preserve.
func buildAccount(f ui.AccountForm, existing *config.Account) (config.Account, []string) {
	var problems []string
	a := config.Account{
		Name: strings.TrimSpace(f.Name), Provider: f.Provider, BaseURL: strings.TrimSpace(f.BaseURL),
		URLs: lines(f.URLs), Include: lines(f.Include), Exclude: lines(f.Exclude), CAFile: strings.TrimSpace(f.CAFile),
		CloneProtocol: f.CloneProtocol, SkipForks: f.SkipForks, PartialClone: f.PartialClone, CleanupMode: f.CleanupMode,
		OAuthClientID: strings.TrimSpace(f.ClientID), OAuthWebURL: strings.TrimSpace(f.WebURL),
	}
	if !f.SkipArchived {
		a.SkipArchived = yes(false) // archived repositories are skipped unless the box is unticked
	}
	for _, p := range []struct {
		label, v string
		dst      *config.Duration
	}{{"Listing interval", f.DiscoveryInterval, &a.DiscoveryInterval}, {"Sync interval", f.Interval, &a.Interval}} {
		if strings.TrimSpace(p.v) == "" {
			continue
		}
		d, err := config.ParseFlexibleDuration(p.v)
		if err != nil {
			problems = append(problems, p.label+": "+err.Error())
			continue
		}
		*p.dst = config.Duration(d)
	}
	switch f.Auth {
	case "file":
		a.TokenFile = strings.TrimSpace(f.TokenFile)
	case "env":
		a.TokenEnv = strings.TrimSpace(f.TokenEnv)
	case "keep":
		if existing != nil {
			a.TokenFile, a.TokenEnv = existing.TokenFile, existing.TokenEnv
			if a.OAuthClientID == "" {
				a.OAuthClientID, a.OAuthWebURL = existing.OAuthClientID, existing.OAuthWebURL
			}
		}
	case "none":
		if f.Provider != "git" {
			problems = append(problems, "Only generic git accounts can work without a credential.")
		}
	case "token", "device":
		// the secret goes to the secret store; the config names no credential source
	default:
		problems = append(problems, "Choose how the credential is supplied.")
	}
	return a, problems
}

func (d *Daemon) providerFor(a config.Account, tok secrets.Token) (provider.Provider, error) {
	hc, err := d.httpFor(a.CAFile)
	if err != nil {
		return nil, err
	}
	return provider.New(provider.Kind(a.Provider), provider.Config{BaseURL: a.BaseURL, Token: tok, HTTP: hc, Remotes: a.URLs})
}

// verify proves the credential works before anything is saved, so a typo never becomes a broken account.
func (d *Daemon) verify(ctx context.Context, a config.Account, tok secrets.Token) (provider.Auth, error) {
	p, err := d.providerFor(a, tok)
	if err != nil {
		return provider.Auth{}, ui.Invalid("Cannot use these account settings: " + err.Error())
	}
	cctx, cancel := context.WithTimeout(ctx, credentialCheckTimeout)
	defer cancel()
	au, err := p.CheckAuth(cctx)
	switch {
	case err == nil:
		return au, nil
	case errors.Is(err, provider.ErrAuth):
		return au, ui.Invalid(a.Provider + " rejected the credential (revoked, expired, or for another host?): " + err.Error())
	default:
		var we *ratelimit.WaitError
		if errors.As(err, &we) {
			return au, ui.Invalid("The platform is rate limiting us; try again after " + we.RetryAt.Format("15:04:05") + ".")
		}
		return au, ui.Invalid("Could not reach the platform to verify the credential: " + err.Error())
	}
}

func (b uiBackend) SaveAccount(ctx context.Context, f ui.AccountForm) error {
	d := b.d
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()

	cfg, err := d.currentConfig()
	if err != nil {
		return err
	}
	var existing *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == strings.TrimSpace(f.Name) {
			existing = &cfg.Accounts[i]
		}
	}
	if f.New && existing != nil {
		return ui.Invalid(fmt.Sprintf("An account named %q already exists; edit it instead.", f.Name))
	}
	if !f.New && existing == nil {
		return ui.Invalid(fmt.Sprintf("Account %q no longer exists.", f.Name))
	}
	if f.Auth == "device" {
		return ui.Invalid("Device login runs as its own flow; use the sign-in button.")
	}
	acct, problems := buildAccount(f, existing)
	if len(problems) > 0 {
		return ui.Invalid(problems...)
	}

	// Clone root: required once any account exists.
	root := strings.TrimSpace(f.Root)
	needRoot := cfg.General.Root == ""
	if needRoot && root == "" {
		return ui.Invalid("Choose the folder repositories are cloned into (an absolute path).")
	}

	// 1. dry-run the config edit: a form that cannot be saved must not touch the secret store
	cur, next, err := d.plan(func(text string) (string, error) {
		if needRoot {
			g := cfg.General
			g.Root = root
			var err error
			if text, err = config.SetGeneral(text, g); err != nil {
				return "", err
			}
		}
		return config.SetAccount(text, acct)
	})
	if err != nil {
		return err
	}

	// 2. work out and verify the credential
	var tok secrets.Token
	storeToken := false
	switch f.Auth {
	case "token":
		if f.Token.IsZero() {
			return ui.Invalid("Paste the access token.")
		}
		tok, storeToken = f.Token, true
	case "file", "env":
		if tok, err = (secrets.Source{Env: acct.TokenEnv, File: acct.TokenFile}).Resolve(d.Secrets); err != nil {
			return ui.Invalid("Cannot read the credential: " + err.Error())
		}
	}
	if !tok.IsZero() {
		d.Redactor.Add(tok.Reveal())
	}
	if !tok.IsZero() || f.Auth == "none" { // "none" still builds the provider, which validates the URL list
		if _, err := d.verify(ctx, acct, tok); err != nil {
			return err
		}
	}

	// 3. apply: secret first (it is useless without the account, harmless if the write fails), then the file
	var previous secrets.Token
	hadPrevious := false
	if storeToken {
		if previous, err = d.Secrets.Get("account/" + acct.Name); err == nil {
			hadPrevious = true
		}
		if err := d.Secrets.Set("account/"+acct.Name, tok); err != nil {
			return ui.Invalid("Could not store the token: " + err.Error() + " (no keychain? choose \"token file\" or \"environment variable\" instead)")
		}
	}
	if err := d.commit(cur, next); err != nil {
		if storeToken && !hadPrevious {
			_ = d.Secrets.Delete("account/" + acct.Name)
		} else if storeToken && hadPrevious {
			_ = d.Secrets.Set("account/"+acct.Name, previous)
		}
		return err
	}
	return nil
}

func (b uiBackend) RemoveAccount(_ context.Context, name string, deleteToken bool) error {
	d := b.d
	cfg, err := d.currentConfig()
	if err != nil {
		return err
	}
	var usedStore bool
	found := false
	for _, a := range cfg.Accounts {
		if a.Name == name {
			found, usedStore = true, a.TokenEnv == "" && a.TokenFile == ""
		}
	}
	if !found {
		return ui.Invalid(fmt.Sprintf("Account %q does not exist.", name))
	}
	if err := d.edit(func(text string) (string, error) { return config.RemoveAccount(text, name) }); err != nil {
		return err
	}
	if deleteToken && usedStore {
		_ = d.Secrets.Delete("account/" + name) // best effort: the account is already gone
	}
	return nil
}

// ---- device login

type deviceFlow struct {
	mu     sync.Mutex
	view   ui.DeviceView
	cancel context.CancelFunc
}

func (f *deviceFlow) set(fn func(v *ui.DeviceView)) {
	f.mu.Lock()
	fn(&f.view)
	f.mu.Unlock()
}

func (f *deviceFlow) snapshot() ui.DeviceView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view
}

func (d *Daemon) pruneDevices() {
	for id, f := range d.devices {
		v := f.snapshot()
		if v.State != "pending" && d.Clock.Now().Sub(v.ExpiresAt) > deviceKeepFor {
			delete(d.devices, id)
		}
	}
}

// safeLinkURL admits only https (or loopback http) verification links; anything else is shown as text, never as a link.
func safeLinkURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname()))) {
		return ""
	}
	return u.String()
}

func (b uiBackend) StartDeviceLogin(ctx context.Context, f ui.AccountForm) (ui.DeviceView, error) {
	d := b.d
	if f.Provider != "github" {
		return ui.DeviceView{}, ui.Invalid("Device login is only available for GitHub. For " + f.Provider + " paste an access token instead.")
	}
	cfg, err := d.currentConfig()
	if err != nil {
		return ui.DeviceView{}, err
	}
	var existing *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == strings.TrimSpace(f.Name) {
			existing = &cfg.Accounts[i]
		}
	}
	if f.New && existing != nil {
		return ui.DeviceView{}, ui.Invalid(fmt.Sprintf("An account named %q already exists; sign in again from its row instead.", f.Name))
	}
	f.Auth = "device"
	acct, problems := buildAccount(f, existing)
	if len(problems) > 0 {
		return ui.DeviceView{}, ui.Invalid(problems...)
	}
	if acct.OAuthClientID == "" {
		return ui.DeviceView{}, ui.Invalid("A client ID is required. Register a GitHub App (device flow enabled, read-only permissions) or an OAuth App and enter its client ID; no secret is needed.")
	}
	if needRoot := cfg.General.Root == "" && strings.TrimSpace(f.Root) == ""; needRoot {
		return ui.DeviceView{}, ui.Invalid("Choose the folder repositories are cloned into (an absolute path).")
	}
	if _, _, err := d.plan(func(text string) (string, error) { // would the finished account be valid?
		if cfg.General.Root == "" {
			g := cfg.General
			g.Root = strings.TrimSpace(f.Root)
			var err error
			if text, err = config.SetGeneral(text, g); err != nil {
				return "", err
			}
		}
		return config.SetAccount(text, acct)
	}); err != nil {
		return ui.DeviceView{}, err
	}

	d.devMu.Lock()
	d.pruneDevices()
	pending := 0
	for _, fl := range d.devices {
		if fl.snapshot().State == "pending" {
			pending++
		}
	}
	d.devMu.Unlock()
	if pending >= maxPendingDevices {
		return ui.DeviceView{}, ui.Invalid("Too many sign-ins are waiting; finish or cancel one first.")
	}

	hc, err := d.httpFor(acct.CAFile)
	if err != nil {
		return ui.DeviceView{}, ui.Invalid(err.Error())
	}
	dev := &github.Device{HTTP: hc, Clock: d.Clock, WebBase: acct.OAuthWebURL, ClientID: acct.OAuthClientID, Scope: strings.TrimSpace(f.Scope)}
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	dc, err := dev.Start(sctx)
	if err != nil {
		return ui.DeviceView{}, ui.Invalid("GitHub did not start the sign-in: " + err.Error())
	}
	life := time.Duration(dc.ExpiresIn) * time.Second
	if life <= 0 || life > time.Hour {
		life = deviceFallbackLifetime
	}
	id := secretsID()
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), life) // outlives this HTTP request
	flow := &deviceFlow{cancel: pcancel, view: ui.DeviceView{
		ID: id, Account: acct.Name, UserCode: dc.UserCode, VerificationURI: safeLinkURL(dc.VerificationURI),
		ExpiresAt: d.Clock.Now().Add(life), State: "pending",
	}}
	if flow.view.VerificationURI == "" {
		flow.view.Message = "Open " + dc.VerificationURI + " in your browser and enter the code."
	}
	d.devMu.Lock()
	d.devices[id] = flow
	d.devMu.Unlock()
	go d.finishDevice(pctx, flow, dev, dc, acct, f)
	return flow.snapshot(), nil
}

func (d *Daemon) finishDevice(ctx context.Context, fl *deviceFlow, dev *github.Device, dc github.DeviceCode, acct config.Account, f ui.AccountForm) {
	defer fl.cancel()
	fail := func(state, msg string) { fl.set(func(v *ui.DeviceView) { v.State, v.Message = state, msg }) }
	tok, err := dev.Poll(ctx, dc)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fail("cancelled", "Sign-in cancelled.")
		} else {
			fail("failed", err.Error())
		}
		return
	}
	d.Redactor.Add(tok.Reveal())
	au, err := d.verify(ctx, acct, tok)
	if err != nil {
		fail("failed", err.Error())
		return
	}
	prev, perr := d.Secrets.Get("account/" + acct.Name)
	if err := d.Secrets.Set("account/"+acct.Name, tok); err != nil {
		fail("failed", "Signed in, but the token could not be stored: "+err.Error()+" (no keychain? use a token file or environment variable)")
		return
	}
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	cur, next, err := d.plan(func(text string) (string, error) {
		cfg, cerr := config.Parse([]byte(text))
		if cerr != nil {
			return "", cerr
		}
		if cfg.General.Root == "" {
			g := cfg.General
			g.Root = strings.TrimSpace(f.Root)
			var err error
			if text, err = config.SetGeneral(text, g); err != nil {
				return "", err
			}
		}
		return config.SetAccount(text, acct)
	})
	if err == nil {
		err = d.commit(cur, next)
	}
	if err != nil {
		if perr == nil {
			_ = d.Secrets.Set("account/"+acct.Name, prev)
		} else {
			_ = d.Secrets.Delete("account/" + acct.Name)
		}
		fail("failed", "Signed in, but the account could not be saved: "+err.Error())
		return
	}
	fl.set(func(v *ui.DeviceView) { v.State, v.Login, v.Message = "done", au.Login, "" })
}

func (b uiBackend) DeviceLogin(id string) (ui.DeviceView, bool) {
	b.d.devMu.Lock()
	defer b.d.devMu.Unlock()
	fl, ok := b.d.devices[id]
	if !ok {
		return ui.DeviceView{}, false
	}
	return fl.snapshot(), true
}

func (b uiBackend) CancelDeviceLogin(id string) {
	b.d.devMu.Lock()
	fl, ok := b.d.devices[id]
	b.d.devMu.Unlock()
	if ok {
		fl.cancel()
	}
}

func secretsID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic("daemon: no system randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
