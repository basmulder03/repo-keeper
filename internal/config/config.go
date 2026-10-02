// SPDX-License-Identifier: Apache-2.0

// Package config loads and validates the TOML configuration; secrets never live here.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/basmulder03/repo-keeper/internal/cleanup"
)

// MinInterval is the hard floor so config can never make repo-keeper abusive (FR-R4).
const MinInterval = 5 * time.Minute

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
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Config is the whole file.
type Config struct {
	General General `toml:"general"`
	Cleanup Cleanup `toml:"cleanup"`
	Repos   []Repo  `toml:"repo"`
}

// General holds scheduling behaviour.
type General struct {
	Interval    Duration `toml:"interval"`     // between syncs of one repo (default 30m, min 5m)
	Concurrency int      `toml:"concurrency"`  // parallel repo syncs (default 4)
	PerHost     int      `toml:"per_host"`     // parallel operations per remote host (default 2)
	QuietHours  string   `toml:"quiet_hours"`  // "23:00-07:00" local time; no scheduled syncs inside
	AllBranches *bool    `toml:"all_branches"` // fetch all branches (default true)
}

// Cleanup is the global branch-cleanup policy.
type Cleanup struct {
	Mode             string   `toml:"mode"` // off | dry-run | auto
	MinAge           Duration `toml:"min_age"`
	Protected        []string `toml:"protected"`
	AllowNeverPushed bool     `toml:"allow_never_pushed"`
}

// Repo is one tracked clone (provider discovery adds more in M3).
type Repo struct {
	Path        string   `toml:"path"`
	Remote      string   `toml:"remote"`
	Interval    Duration `toml:"interval"`
	AllBranches *bool    `toml:"all_branches"`
	CleanupMode string   `toml:"cleanup"` // overrides [cleanup].mode for this repo
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
	b, err := os.ReadFile(path) //nolint:gosec // see #nosec above
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
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
