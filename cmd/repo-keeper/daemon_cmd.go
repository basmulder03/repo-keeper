// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/daemon"
	"github.com/basmulder03/repo-keeper/internal/instance"
	"github.com/basmulder03/repo-keeper/internal/obs"
	"github.com/basmulder03/repo-keeper/internal/paths"
	"github.com/basmulder03/repo-keeper/internal/store"
)

type commonFlags struct {
	configPath string
	stateDir   string
}

func (a *app) resolve(c *commonFlags) error {
	var err error
	if c.configPath == "" {
		if c.configPath, err = paths.ConfigPath(); err != nil {
			return err
		}
	}
	if c.stateDir == "" {
		if c.stateDir, err = paths.StateDir(); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) cmdInit(_ context.Context, args []string) int {
	fs := a.newFlagSet("init")
	var c commonFlags
	fs.StringVar(&c.configPath, "config", "", "config file (default: per-user config dir)")
	if fs.Parse(args) != nil {
		return 2
	}
	if err := a.resolve(&c); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(c.configPath), 0o700); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	f, err := os.OpenFile(c.configPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // #nosec G304 -- user-chosen config path
	if errors.Is(err, os.ErrExist) {
		_, _ = fmt.Fprintf(a.err, "%s already exists; not overwriting\n", c.configPath)
		return 1
	}
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	_, werr := f.WriteString(config.Starter)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_, _ = fmt.Fprintln(a.err, werr)
		return 1
	}
	a.printf("wrote %s\nAdd [[repo]] entries, then run: repo-keeper daemon\n", c.configPath)
	return 0
}

func (a *app) cmdConfig(_ context.Context, args []string) int {
	if len(args) == 0 || args[0] != "validate" {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper config validate [--config FILE]")
		return 2
	}
	fs := a.newFlagSet("config validate")
	var c commonFlags
	fs.StringVar(&c.configPath, "config", "", "config file")
	if fs.Parse(args[1:]) != nil {
		return 2
	}
	if err := a.resolve(&c); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	cfg, err := config.Load(c.configPath)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	a.printf("%s: valid (%d repositories)\n", c.configPath, len(cfg.Repos))
	return 0
}

func (a *app) cmdDaemon(ctx context.Context, args []string) int {
	fs := a.newFlagSet("daemon")
	var c commonFlags
	fs.StringVar(&c.configPath, "config", "", "config file (default: per-user config dir)")
	fs.StringVar(&c.stateDir, "state-dir", "", "state directory (default: per-user state dir)")
	level := fs.String("log-level", "info", "debug | info | warn | error")
	jsonLog := fs.Bool("json", false, "JSON logs")
	if fs.Parse(args) != nil {
		return 2
	}
	if err := a.resolve(&c); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*level)); err != nil {
		_, _ = fmt.Fprintf(a.err, "invalid --log-level %q\n", *level)
		return 2
	}
	runner, err := a.newRunner()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	red := &obs.Redactor{}
	d := &daemon.Daemon{
		ConfigPath: c.configPath, StateDir: c.stateDir, Runner: runner, Clock: a.clock,
		Version: version, Secrets: a.secrets, Redactor: red, Log: obs.New(a.err, lvl, *jsonLog, red),
	}
	if err := d.Run(ctx); err != nil {
		if errors.Is(err, instance.ErrRunning) {
			_, _ = fmt.Fprintln(a.err, err)
			return 3
		}
		_, _ = fmt.Fprintln(a.err, "repo-keeper:", err)
		return 1
	}
	return 0
}

func (a *app) cmdStatus(ctx context.Context, args []string) int {
	fs := a.newFlagSet("status")
	var c commonFlags
	fs.StringVar(&c.stateDir, "state-dir", "", "state directory")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if fs.Parse(args) != nil {
		return 2
	}
	if err := a.resolve(&c); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	dbPath, _, _ := daemon.StatePaths(c.stateDir)
	if _, err := os.Stat(dbPath); err != nil {
		_, _ = fmt.Fprintln(a.err, "no state yet: start the daemon first (repo-keeper daemon)")
		return 1
	}
	st, err := store.Open(dbPath)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	defer func() { _ = st.Close() }()
	repos, err := st.ListRepos(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	if *asJSON {
		type row struct {
			Path, Host, DefaultBranch, Status, Reason, Error, FastForward string
			LastSync, NextSync                                            time.Time
			Failures                                                      int
			NeedsAttention                                                bool
		}
		rows := make([]row, 0, len(repos))
		for _, r := range repos {
			rows = append(rows, row{r.Path, r.Host, r.DefaultBranch, r.LastStatus, r.LastReason, r.LastError, r.LastFF, r.LastSync, r.NextSync, r.Failures, r.NeedsAttention})
		}
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		return 0
	}
	tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REPO\tDEFAULT\tSTATUS\tLAST SYNC\tNEXT SYNC")
	for _, r := range repos {
		st := r.LastStatus
		switch {
		case st == "":
			st = "pending"
		case r.NeedsAttention:
			st += " (needs attention: " + strings.TrimSpace(r.LastReason) + ")"
		case r.LastFF != "" && r.LastFF != "up-to-date" && r.LastFF != "fast-forwarded":
			st += " (" + r.LastFF + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Path, r.DefaultBranch, st, ago(r.LastSync), until(r.NextSync))
	}
	_ = tw.Flush()
	return 0
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

func until(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	if d := time.Until(t); d > 0 {
		return "in " + d.Round(time.Second).String()
	}
	return "due"
}
