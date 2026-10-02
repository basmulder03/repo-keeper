// SPDX-License-Identifier: Apache-2.0

// Package ui serves the local web interface: loopback only, session + CSRF protected, no external assets.
package ui

import (
	"context"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/control"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/store"
)

// Info describes the running daemon.
type Info struct {
	Version    string
	GitVersion string
	GoVersion  string
	ConfigPath string
	StateDir   string
	UIAddr     string
	Started    time.Time
}

// CleanupView is one repository's latest cleanup evaluation.
type CleanupView struct {
	Repo   store.Repo
	Mode   string
	Report cleanup.Report
}

// Status is the compact summary served to the tray helper.
type Status = control.Status

// Backend is everything the UI may ask of the daemon; the UI never touches git, the DB or secrets itself.
type Backend interface {
	Info() Info
	Repos(ctx context.Context) ([]store.Repo, error)
	Repo(ctx context.Context, id int64) (store.Repo, error)
	Runs(ctx context.Context, id int64, n int) ([]store.Run, error)
	Accounts(ctx context.Context) ([]store.Account, error)
	Events(ctx context.Context, n int) ([]store.Event, error)
	Hosts() []ratelimit.HostState
	Commands() []gitx.CommandRecord
	Cleanups(ctx context.Context) ([]CleanupView, error)
	Audit(ctx context.Context, n int) ([]audit.Entry, error)
	Trash(ctx context.Context, id int64) ([]gitx.TrashRef, error)

	Status(ctx context.Context) (Status, error)
	SyncAll(ctx context.Context) error
	SetPaused(paused bool)
	SyncNow(ctx context.Context, id int64) error
	DiscoverNow(ctx context.Context, account string) error
	CleanupNow(ctx context.Context, id int64) (cleanup.Report, error)
	Restore(ctx context.Context, id int64, branch string) error

	// Config returns the file text and a version token for conflict detection.
	Config() (text, version string, err error)
	// SaveConfig validates then atomically replaces the file; it fails if version no longer matches.
	SaveConfig(ctx context.Context, text, version string) error
	Bundle(ctx context.Context) ([]byte, error)
}
