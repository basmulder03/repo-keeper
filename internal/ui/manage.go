// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

// ValidationError is a mistake the user can fix; the UI re-renders the form with these problems (HTTP 422).
type ValidationError struct{ Problems []string }

// Error implements error.
func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// Invalid builds a ValidationError.
func Invalid(problems ...string) *ValidationError { return &ValidationError{Problems: problems} }

// AccountForm is what the add/edit-account form submits. Strings stay strings so a rejected form can be shown
// again exactly as typed; the backend parses and validates.
type AccountForm struct {
	New      bool
	Name     string
	Provider string
	BaseURL  string

	// Auth is how the credential is supplied: token (pasted, stored in the secret store), device (GitHub device
	// flow), file (token_file), env (token_env) or keep (edit only: leave the credential alone).
	Auth      string
	Token     secrets.Token // pasted secret; never echoed back, never logged
	TokenFile string
	TokenEnv  string
	ClientID  string // device flow
	WebURL    string // device flow, GitHub Enterprise Server web root
	Scope     string // device flow, OAuth Apps only (empty for GitHub Apps)

	URLs              string // generic git only: one clone URL per line
	Include, Exclude  string // one pattern per line (commas also accepted)
	CAFile            string
	CloneProtocol     string // "", https, ssh
	SkipArchived      bool
	SkipForks         bool
	PartialClone      bool
	DiscoveryInterval string // "", 12h, 1d ...
	Interval          string
	CleanupMode       string // "" = inherit, off, dry-run, auto
	Root              string // only asked for when no clone root is configured yet
}

// SettingsForm is the general / cleanup / UI settings form.
type SettingsForm struct {
	Root, Interval, Concurrency, PerHost, QuietHours string
	AllBranches                                      bool
	Secrets, SecretsFile                             string // keyring | file

	CleanupMode, MinAge, Protected string
	AllowNeverPushed               bool

	UIEnabled bool
	UIPort    string
}

// RepoForm adds one repository that is listed directly in the config.
type RepoForm struct {
	Path, Remote, Interval, CleanupMode string
	AllBranches                         string // "" inherit, "true", "false"
}

// SettingsView is the settings page's data.
type SettingsView struct {
	Form      SettingsForm
	Repos     []config.Repo
	ReadOnly  string // why editing is disabled, "" = editable
	Providers []string
}

// DeviceView is the state of one OAuth device-flow login.
type DeviceView struct {
	ID              string
	Account         string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	State           string // pending | done | failed | cancelled
	Message         string
	Login           string // set when done
}

// Manage is the configuration surface of the daemon. Every method either fully applies a change (config file
// edited atomically with a backup, secret stored, daemon reloaded) or changes nothing.
type Manage interface {
	Settings(ctx context.Context) (SettingsView, error)
	SaveSettings(ctx context.Context, f SettingsForm) (restartNeeded bool, err error)
	AddRepo(ctx context.Context, f RepoForm) error
	RemoveRepo(ctx context.Context, path string) error

	AccountForm(ctx context.Context, name string) (AccountForm, bool, error)
	SaveAccount(ctx context.Context, f AccountForm) error
	RemoveAccount(ctx context.Context, name string, deleteToken bool) error

	StartDeviceLogin(ctx context.Context, f AccountForm) (DeviceView, error)
	DeviceLogin(id string) (DeviceView, bool)
	CancelDeviceLogin(id string)
}
