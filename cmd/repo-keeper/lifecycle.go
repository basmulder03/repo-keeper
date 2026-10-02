// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/paths"
	"github.com/basmulder03/repo-keeper/internal/tray"
)

const (
	startWait    = 10 * time.Second
	stopWait     = 20 * time.Second
	maxLogBytes  = 10 << 20
	lifecycleTip = "For a supervised service use systemd / Home Manager instead (docs/INSTALL.md); start/stop are for setups without one."
)

type lifecycleFlags struct {
	commonFlags
	runtimeDir string
	logLevel   string
	noUI       bool
	jsonLog    bool
}

func (a *app) lifecycleFlagSet(name string, lf *lifecycleFlags) *flag.FlagSet {
	fs := a.newFlagSet(name)
	fs.StringVar(&lf.configPath, "config", "", "config file (default: per-user config dir)")
	fs.StringVar(&lf.stateDir, "state-dir", "", "state directory (default: per-user state dir)")
	fs.StringVar(&lf.runtimeDir, "runtime-dir", "", "directory holding ui.json (default: per-user runtime dir)")
	fs.StringVar(&lf.logLevel, "log-level", "info", "debug | info | warn | error")
	fs.BoolVar(&lf.noUI, "no-ui", false, "do not start the web interface")
	fs.BoolVar(&lf.jsonLog, "json", false, "JSON logs")
	return fs
}

func (a *app) lifecycleClient(lf *lifecycleFlags) (*tray.Client, error) {
	dir := lf.runtimeDir
	if dir == "" {
		var err error
		if dir, err = paths.RuntimeDir(); err != nil {
			return nil, err
		}
	}
	return &tray.Client{RuntimeDir: dir}, nil
}

func (a *app) cmdStart(ctx context.Context, args []string) int {
	var lf lifecycleFlags
	fs := a.lifecycleFlagSet("start", &lf)
	if fs.Parse(args) != nil {
		return 2
	}
	if err := a.resolve(&lf.commonFlags); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	c, err := a.lifecycleClient(&lf)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	if st, err := c.Status(ctx); err == nil {
		a.printf("already running (version %s, %d repositories). Open the interface with: repo-keeper ui\n", st.Version, st.Repos)
		return 0
	}
	logPath := filepath.Join(lf.stateDir, "daemon.log")
	child, err := a.spawnDaemon(&lf, logPath)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "could not start the daemon:", err)
		return 1
	}
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		select {
		case werr := <-child.exited:
			_, _ = fmt.Fprintf(a.err, "the daemon exited right away (%v). Last log lines from %s:\n%s", werr, logPath, tailFile(logPath, 8))
			return 1
		case <-ctx.Done():
			return 1
		case <-time.After(100 * time.Millisecond):
		}
		if st, err := c.Status(ctx); err == nil {
			_ = child.cmd.Process.Release() // let it run on its own
			a.printf("started (pid %d, version %s, %d repositories)\nlog: %s\nopen the interface: repo-keeper ui\nstop it: repo-keeper stop\n%s\n",
				child.cmd.Process.Pid, st.Version, st.Repos, logPath, lifecycleTip)
			return 0
		}
	}
	_, _ = fmt.Fprintf(a.err, "the daemon did not become ready within %s. Last log lines from %s:\n%s", startWait, logPath, tailFile(logPath, 8))
	return 1
}

type spawned struct {
	cmd    *exec.Cmd
	exited chan error
}

// spawnDaemon starts `repo-keeper daemon ...` detached, logging to a private, size-capped file.
func (a *app) spawnDaemon(lf *lifecycleFlags, logPath string) (*spawned, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Stat(logPath); err == nil && st.Size() > maxLogBytes {
		_ = os.Rename(logPath, logPath+".1") // one generation is enough for a debugging aid
	}
	// #nosec G304 -- our own state dir
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // see #nosec above
	if err != nil {
		return nil, err
	}
	defer func() { _ = logf.Close() }() // the child keeps its own descriptor
	args := []string{"daemon", "--config", lf.configPath, "--state-dir", lf.stateDir, "--log-level", lf.logLevel}
	if lf.noUI {
		args = append(args, "--no-ui")
	}
	if lf.jsonLog {
		args = append(args, "--json")
	}
	// #nosec G204 -- this very binary, fixed subcommand, flags from our own parser
	cmd := exec.Command(exe, args...) //nolint:forbidigo,gosec,noctx // sanctioned: our own daemon; deliberately not tied to a context, it must outlive this process
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return &spawned{cmd: cmd, exited: exited}, nil
}

func tailFile(path string, n int) string {
	// #nosec G304 -- our own log
	b, err := os.ReadFile(path) //nolint:gosec // see #nosec above
	if err != nil {
		return "  (no log)\n"
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var out strings.Builder
	for _, l := range lines {
		out.WriteString("  " + string(l) + "\n")
	}
	return out.String()
}

func (a *app) cmdStop(ctx context.Context, args []string) int {
	var lf lifecycleFlags
	fs := a.lifecycleFlagSet("stop", &lf)
	if fs.Parse(args) != nil {
		return 2
	}
	c, err := a.lifecycleClient(&lf)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	if _, err := c.Status(ctx); err != nil {
		a.printf("not running\n")
		return 0
	}
	if err := c.Shutdown(ctx, false); err != nil {
		_, _ = fmt.Fprintln(a.err, "could not ask the daemon to stop:", err)
		return 1
	}
	if !a.waitFor(ctx, stopWait, func() bool { _, err := c.Status(ctx); return errors.Is(err, tray.ErrNotRunning) }) {
		_, _ = fmt.Fprintf(a.err, "the daemon is still running after %s (a long sync may be finishing); try again or stop the service manager unit\n", stopWait)
		return 1
	}
	a.printf("stopped\n")
	return 0
}

func (a *app) cmdRestart(ctx context.Context, args []string) int {
	var lf lifecycleFlags
	fs := a.lifecycleFlagSet("restart", &lf)
	if fs.Parse(args) != nil {
		return 2
	}
	c, err := a.lifecycleClient(&lf)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	before, rerr := c.Runtime()
	if _, err := c.Status(ctx); err != nil || rerr != nil {
		return a.cmdStart(ctx, args) // nothing to restart: just start
	}
	if err := c.Shutdown(ctx, true); err != nil {
		_, _ = fmt.Fprintln(a.err, "could not ask the daemon to restart:", err)
		return 1
	}
	// the daemon re-executes in place; a new runtime file (new control token) marks the new instance
	ok := a.waitFor(ctx, stopWait, func() bool {
		now, err := c.Runtime()
		if err != nil || now.Control == before.Control {
			return false
		}
		_, err = c.Status(ctx)
		return err == nil
	})
	if !ok {
		_, _ = fmt.Fprintln(a.err, "the daemon did not come back; check its log or start it with: repo-keeper start")
		return 1
	}
	a.printf("restarted\n")
	return 0
}

func (a *app) waitFor(ctx context.Context, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
	}
	return cond()
}
