// SPDX-License-Identifier: Apache-2.0

// Package config loads and validates the TOML configuration; secrets never live here.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/provider"
	_ "github.com/basmulder03/repo-keeper/internal/provider/all" // registers every platform for validation
)

// MinInterval is the hard floor so config can never make repo-keeper abusive (FR-R4).
const MinInterval = 5 * time.Minute

// maxConfigBytes bounds what we will read and parse; real configs are a few KiB.
const maxConfigBytes = 1 << 20

// MinDiscoveryInterval keeps repository listing (many API calls) politely infrequent.
const MinDiscoveryInterval = time.Hour

var clientIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{4,64}$`)

var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func isLoopback(h string) bool { ip := net.ParseIP(h); return ip != nil && ip.IsLoopback() }

// Duration is a TOML-friendly time.Duration ("30m", "7d" is not supported by Go; use "168h").
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(FormatDuration(time.Duration(d))), nil }

// Config is the whole file.
type Config struct {
	General  General   `toml:"general,omitempty"`
	Cleanup  Cleanup   `toml:"cleanup,omitempty"`
	Repos    []Repo    `toml:"repo,omitempty"`
	Accounts []Account `toml:"account,omitempty"`
	UI       UI        `toml:"ui,omitempty"`
}

// General holds scheduling behaviour.
type General struct {
	Interval    Duration `toml:"interval,omitempty"`     // between syncs of one repo (default 30m, min 5m)
	Concurrency int      `toml:"concurrency,omitempty"`  // parallel repo syncs (default 4)
	PerHost     int      `toml:"per_host,omitempty"`     // parallel operations per remote host (default 2)
	QuietHours  string   `toml:"quiet_hours,omitempty"`  // "23:00-07:00" local time; no scheduled syncs inside
	AllBranches *bool    `toml:"all_branches,omitempty"` // fetch all branches (default true)
	Secrets     string   `toml:"secrets,omitempty"`      // keyring (default) | file (passphrase-encrypted; headless hosts)
	SecretsFile string   `toml:"secrets_file,omitempty"` // default <state dir>/secrets.enc
	Root        string   `toml:"root,omitempty"`         // clone root for account-discovered repos (absolute)
}

// Cleanup is the global branch-cleanup policy.
type Cleanup struct {
	Mode             string   `toml:"mode,omitempty"`                    // off | dry-run | auto
	MinAge           Duration `toml:"min_age,omitempty" render:"always"` // 0 is meaningful here (the default is 7d), so it is always written
	Protected        []string `toml:"protected,omitempty"`
	AllowNeverPushed bool     `toml:"allow_never_pushed,omitempty"`
}

// UI configures the local web interface.
type UI struct {
	Enabled *bool `toml:"enabled,omitempty"` // default true
	Port    int   `toml:"port,omitempty"`    // preferred loopback port (default 7878; a free port is used if taken)
}

// Account is a platform login whose repositories are discovered and cloned automatically.
type Account struct {
	Name     string `toml:"name,omitempty"`
	Provider string `toml:"provider,omitempty"` // github | gitlab | gitea | forgejo
	BaseURL  string `toml:"base_url,omitempty"` // API base: GHES https://ghe.example.com/api/v3, GitLab https://gitlab.example.com, Gitea/Forgejo https://git.example.com (required); default is the public cloud
	CAFile   string `toml:"ca_file,omitempty"`  // PEM bundle with the private CA of a self-hosted instance (absolute path)
	// Credential source; with neither set the OS keychain entry "account/<name>" is used.
	TokenEnv          string   `toml:"token_env,omitempty"`
	TokenFile         string   `toml:"token_file,omitempty"`
	Include           []string `toml:"include,omitempty"` // globs on owner/name; empty = everything
	Exclude           []string `toml:"exclude,omitempty"`
	SkipArchived      *bool    `toml:"skip_archived,omitempty"` // default true
	SkipForks         bool     `toml:"skip_forks,omitempty"`
	CloneProtocol     string   `toml:"clone_protocol,omitempty"`     // https (default) | ssh
	PartialClone      bool     `toml:"partial_clone,omitempty"`      // blobless clones
	DiscoveryInterval Duration `toml:"discovery_interval,omitempty"` // default 6h, min 1h
	Interval          Duration `toml:"interval,omitempty"`           // per-repo sync interval override
	CleanupMode       string   `toml:"cleanup,omitempty"`            // overrides [cleanup].mode
	OAuthClientID     string   `toml:"oauth_client_id,omitempty"`    // GitHub device flow: client id of your OAuth/GitHub App (no secret needed)
	OAuthWebURL       string   `toml:"oauth_web_url,omitempty"`      // GitHub Enterprise Server web root for the device flow, e.g. https://ghe.example.com
}

// Repo is one tracked clone (provider discovery adds more in M3).
type Repo struct {
	Path        string   `toml:"path,omitempty"`
	Remote      string   `toml:"remote,omitempty"`
	Interval    Duration `toml:"interval,omitempty"`
	AllBranches *bool    `toml:"all_branches,omitempty"`
	CleanupMode string   `toml:"cleanup,omitempty"` // overrides [cleanup].mode for this repo
}

// Default returns the built-in configuration.
func Default() Config {
	t := true
	return Config{
		General: General{Interval: Duration(30 * time.Minute), Concurrency: 4, PerHost: 2, AllBranches: &t},
		Cleanup: Cleanup{Mode: string(cleanup.ModeDryRun), MinAge: Duration(7 * 24 * time.Hour), Protected: append([]string{}, cleanup.DefaultProtected...)},
	}
}

// Parse decodes and validates TOML; unknown keys are errors so typos never silently weaken a policy.
func Parse(b []byte) (Config, error) {
	c := Default()
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var se *toml.StrictMissingError
		if errors.As(err, &se) {
			return Config{}, fmt.Errorf("config: %s", se.String())
		}
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Load reads and parses the file at path.
func Load(path string) (Config, error) {
	// #nosec G304 -- user-chosen config path
	f, err := os.Open(path) //nolint:gosec // see #nosec above
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if len(b) > maxConfigBytes {
		return Config{}, fmt.Errorf("config: %s is larger than %d KiB", path, maxConfigBytes>>10)
	}
	return Parse(b)
}

// Validate checks every field and returns all problems at once.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if time.Duration(c.General.Interval) < MinInterval {
		bad("general.interval %v is below the %v minimum", time.Duration(c.General.Interval), MinInterval)
	}
	if c.General.Concurrency < 1 || c.General.Concurrency > 32 {
		bad("general.concurrency must be 1..32")
	}
	if c.General.PerHost < 1 || c.General.PerHost > 8 {
		bad("general.per_host must be 1..8")
	}
	if c.General.QuietHours != "" {
		if _, err := ParseQuietHours(c.General.QuietHours); err != nil {
			bad("general.quiet_hours: %v", err)
		}
	}
	if !validMode(c.Cleanup.Mode) {
		bad("cleanup.mode %q must be off, dry-run or auto", c.Cleanup.Mode)
	}
	if c.Cleanup.MinAge < 0 {
		bad("cleanup.min_age must not be negative")
	}
	if c.General.Secrets != "" && c.General.Secrets != "keyring" && c.General.Secrets != "file" {
		bad("general.secrets %q must be keyring or file", c.General.Secrets)
	}
	if c.General.SecretsFile != "" && !filepath.IsAbs(c.General.SecretsFile) {
		bad("general.secrets_file must be an absolute path")
	}
	if c.UI.Port < 0 || c.UI.Port > 65535 {
		bad("ui.port must be 0..65535")
	}
	if c.General.Root != "" && !filepath.IsAbs(c.General.Root) {
		bad("general.root must be an absolute path")
	}
	if len(c.Accounts) > 0 && c.General.Root == "" {
		bad("general.root is required when accounts are configured")
	}
	names := map[string]bool{}
	for i, a := range c.Accounts {
		if !accountName.MatchString(a.Name) {
			bad("account[%d].name %q must match [a-z0-9][a-z0-9_-]*", i, a.Name)
		}
		if names[a.Name] {
			bad("account[%d].name %q is used twice", i, a.Name)
		}
		names[a.Name] = true
		if !provider.Known(provider.Kind(a.Provider)) {
			bad("account[%d].provider %q is not supported (available: %s)", i, a.Provider, strings.Join(provider.Kinds(), ", "))
		}
		if (a.Provider == "gitea" || a.Provider == "forgejo") && a.BaseURL == "" {
			bad("account[%d].base_url is required for %s (the address of your server, e.g. https://codeberg.org)", i, a.Provider)
		}
		if a.TokenEnv != "" && a.TokenFile != "" {
			bad("account[%d]: set only one of token_env and token_file", i)
		}
		if a.OAuthClientID != "" && (a.Provider != "github" || !clientIDRe.MatchString(a.OAuthClientID)) {
			bad("account[%d].oauth_client_id is only for github accounts and must be 4-64 letters, digits, dots, dashes or underscores", i)
		}
		if a.OAuthWebURL != "" {
			if u, err := url.Parse(a.OAuthWebURL); err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname()))) {
				bad("account[%d].oauth_web_url must be https://...", i)
			}
		}
		if a.CAFile != "" && !filepath.IsAbs(a.CAFile) {
			bad("account[%d].ca_file must be an absolute path", i)
		}
		if a.TokenFile != "" && !filepath.IsAbs(a.TokenFile) {
			bad("account[%d].token_file must be an absolute path", i)
		}
		if a.BaseURL != "" {
			if u, err := url.Parse(a.BaseURL); err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname()))) {
				bad("account[%d].base_url must be https://... (plain http only for loopback)", i)
			}
		}
		if a.CloneProtocol != "" && a.CloneProtocol != "https" && a.CloneProtocol != "ssh" {
			bad("account[%d].clone_protocol must be https or ssh", i)
		}
		if a.DiscoveryInterval != 0 && time.Duration(a.DiscoveryInterval) < MinDiscoveryInterval {
			bad("account[%d].discovery_interval is below the %v minimum", i, MinDiscoveryInterval)
		}
		if a.Interval != 0 && time.Duration(a.Interval) < MinInterval {
			bad("account[%d].interval is below the %v minimum", i, MinInterval)
		}
		if a.CleanupMode != "" && !validMode(a.CleanupMode) {
			bad("account[%d].cleanup %q must be off, dry-run or auto", i, a.CleanupMode)
		}
		for _, g := range append(append([]string{}, a.Include...), a.Exclude...) {
			if err := provider.ValidGlob(g); err != nil {
				bad("account[%d]: invalid glob %q: %v", i, g, err)
			}
		}
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		if r.Path == "" || !filepath.IsAbs(r.Path) {
			bad("repo[%d].path must be an absolute path", i)
			continue
		}
		p := filepath.Clean(r.Path)
		if seen[p] {
			bad("repo[%d].path %s is listed twice", i, p)
		}
		seen[p] = true
		if r.Interval != 0 && time.Duration(r.Interval) < MinInterval {
			bad("repo[%d].interval is below the %v minimum", i, MinInterval)
		}
		if r.CleanupMode != "" && !validMode(r.CleanupMode) {
			bad("repo[%d].cleanup %q must be off, dry-run or auto", i, r.CleanupMode)
		}
	}
	return errors.Join(errs...)
}

func validMode(m string) bool {
	return m == string(cleanup.ModeOff) || m == string(cleanup.ModeDryRun) || m == string(cleanup.ModeAuto)
}

// RepoSettings is a repo with every default resolved.
type RepoSettings struct {
	Path        string
	Remote      string
	Interval    time.Duration
	AllBranches bool
	Policy      cleanup.Policy
}

// Resolve merges global defaults into one repo's settings.
func (c Config) Resolve(r Repo) RepoSettings {
	s := RepoSettings{
		Path: filepath.Clean(r.Path), Remote: r.Remote, Interval: time.Duration(c.General.Interval),
		AllBranches: c.General.AllBranches == nil || *c.General.AllBranches,
		Policy: cleanup.Policy{
			Mode: cleanup.Mode(c.Cleanup.Mode), MinAge: time.Duration(c.Cleanup.MinAge),
			Protected: c.Cleanup.Protected, AllowNeverPushed: c.Cleanup.AllowNeverPushed,
		},
	}
	if s.Remote == "" {
		s.Remote = "origin"
	}
	if r.Interval != 0 {
		s.Interval = time.Duration(r.Interval)
	}
	if r.AllBranches != nil {
		s.AllBranches = *r.AllBranches
	}
	if r.CleanupMode != "" {
		s.Policy.Mode = cleanup.Mode(r.CleanupMode)
	}
	return s
}

// Starter is the file written by `repo-keeper init`.
const Starter = `# repo-keeper configuration. Secrets never go in this file.
[general]
interval = "30m"        # how often each repo is synced (minimum 5m)
concurrency = 4         # repos synced in parallel
per_host = 2            # parallel operations per remote host
# quiet_hours = "23:00-07:00"

[cleanup]
mode = "dry-run"        # off | dry-run | auto. Only local branches are ever touched.
min_age = "168h"        # never delete branches whose newest commit is younger than this
protected = ["main", "master", "trunk", "develop", "dev", "staging", "production", "release/*", "hotfix/*"]

# [[repo]]
# path = "/home/you/code/project"
# cleanup = "auto"      # per-repo override

# Discover and clone everything an account can access (repo-keeper accounts add ...):
# [general]
# root = "/home/you/code"
# [[account]]
# name = "personal"
# provider = "github"
# include = ["me/*", "my-org/*"]
# exclude = ["*/archive-*"]
`

// QuietHours is a daily window during which scheduled syncs pause.
type QuietHours struct{ start, end int } // minutes since midnight; start == end means never

// ParseQuietHours parses "HH:MM-HH:MM"; the window may wrap midnight.
func ParseQuietHours(s string) (QuietHours, error) {
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return QuietHours{}, errors.New(`want "HH:MM-HH:MM"`)
	}
	parse := func(v string) (int, error) {
		t, err := time.Parse("15:04", strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("invalid time %q", v)
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	a, err := parse(from)
	if err != nil {
		return QuietHours{}, err
	}
	b, err := parse(to)
	if err != nil {
		return QuietHours{}, err
	}
	return QuietHours{a, b}, nil
}

// Contains reports whether t (local clock) falls inside the window.
func (q QuietHours) Contains(t time.Time) bool {
	if q.start == q.end {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if q.start < q.end {
		return m >= q.start && m < q.end
	}
	return m >= q.start || m < q.end
}

// AccountSettings is an account with every default resolved.
type AccountSettings struct {
	Account
	SkipArchivedRepos bool
	Discovery         time.Duration
	SyncInterval      time.Duration
	Policy            cleanup.Policy
	UseSSH            bool
}

// ResolveAccount merges global defaults into one account's settings.
func (c Config) ResolveAccount(a Account) AccountSettings {
	s := AccountSettings{
		Account: a, SkipArchivedRepos: a.SkipArchived == nil || *a.SkipArchived,
		Discovery: 6 * time.Hour, SyncInterval: time.Duration(c.General.Interval), UseSSH: a.CloneProtocol == "ssh",
		Policy: cleanup.Policy{
			Mode: cleanup.Mode(c.Cleanup.Mode), MinAge: time.Duration(c.Cleanup.MinAge),
			Protected: c.Cleanup.Protected, AllowNeverPushed: c.Cleanup.AllowNeverPushed,
		},
	}
	if a.DiscoveryInterval != 0 {
		s.Discovery = time.Duration(a.DiscoveryInterval)
	}
	if a.Interval != 0 {
		s.SyncInterval = time.Duration(a.Interval)
	}
	if a.CleanupMode != "" {
		s.Policy.Mode = cleanup.Mode(a.CleanupMode)
	}
	return s
}

// UIEnabled reports whether the web UI should start.
func (c Config) UIEnabled() bool { return c.UI.Enabled == nil || *c.UI.Enabled }

// UIPort returns the preferred UI port.
func (c Config) UIPort() int {
	if c.UI.Port == 0 {
		return 7878
	}
	return c.UI.Port
}
