// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/instance"
	"github.com/basmulder03/repo-keeper/internal/sched"
	"github.com/basmulder03/repo-keeper/internal/secrets"
	"github.com/basmulder03/repo-keeper/internal/store"
	"github.com/basmulder03/repo-keeper/internal/syncer"
)

func TestHostOf(t *testing.T) {
	tests := map[string]string{
		"https://github.com/o/r.git":        "github.com",
		"https://user:pw@GitLab.com:8443/x": "gitlab.com:8443",
		"git@github.com:o/r.git":            "github.com",
		"ssh://git@host.example/o/r":        "host.example",
		"/srv/git/repo.git":                 "local",
		"../relative/repo":                  "local",
		`C:\repos\x`:                        "local",
		"":                                  "local",
	}
	for in, want := range tests {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	ge := func(stderr string) error { return &gitx.Error{Stderr: stderr, ExitCode: 128} }
	tests := []struct {
		name string
		r    syncer.Result
		want sched.Kind
	}{
		{"ok", syncer.Result{Status: syncer.OK}, sched.Success},
		{"dirty skip is success", syncer.Result{Status: syncer.Skipped, Reason: "x"}, sched.Success},
		{"locked", syncer.Result{Status: syncer.Skipped, Reason: "locked"}, sched.Busy},
		{"unsafe config", syncer.Result{Status: syncer.Failed, Reason: "unsafe-config", Err: ge("x")}, sched.NeedsUser},
		{"rate limited", syncer.Result{Status: syncer.Failed, Reason: "fetch", Err: ge("remote: HTTP 429 Too Many Requests")}, sched.RateLimited},
		{"auth", syncer.Result{Status: syncer.Failed, Reason: "ls-remote", Err: ge("fatal: Authentication failed for 'https://x'")}, sched.NeedsUser},
		{"ssh auth", syncer.Result{Status: syncer.Failed, Reason: "fetch", Err: ge("git@x: Permission denied (publickey).")}, sched.NeedsUser},
		{"network", syncer.Result{Status: syncer.Failed, Reason: "fetch", Err: ge("Could not resolve host")}, sched.Transient},
		{"local failure", syncer.Result{Status: syncer.Failed, Reason: "fast-forward", Err: ge("Rate limit text in a local error")}, sched.Transient},
	}
	for _, tc := range tests {
		if got := classify(tc.r).kind; got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	if v := classify(syncer.Result{Status: syncer.Failed, Reason: "fetch", Err: errors.New("net down")}); v.resp.Err == nil {
		t.Error("transient network failure must feed the limiter as an error")
	}
	if v := classify(syncer.Result{Status: syncer.Failed, Reason: "fetch", Err: ge("Authentication failed")}); v.resp.Err != nil || v.resp.RateLimited {
		t.Error("auth failures must not penalise the host")
	}
}

type rig struct {
	e       *gitxtest.Env
	clk     *clock.Fake
	d       *Daemon
	cfgPath string
	secrets *secrets.Mem
	cancel  context.CancelFunc
	done    chan error
}

func writeCfg(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cfgFor(repos ...string) string {
	var b strings.Builder
	b.WriteString("[general]\ninterval = \"30m\"\n[cleanup]\nmode = \"off\"\n")
	for _, r := range repos {
		b.WriteString("[[repo]]\npath = " + strconv.Quote(r) + "\n")
	}
	return b.String()
}

func start(t *testing.T, e *gitxtest.Env, cfg string) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{e: e, clk: clock.NewFake(time.Now()), cfgPath: filepath.Join(dir, "config.toml"), done: make(chan error, 1)}
	writeCfg(t, r.cfgPath, cfg)
	r.secrets = &secrets.Mem{}
	r.d = &Daemon{ConfigPath: r.cfgPath, StateDir: filepath.Join(dir, "state"), Runner: e.R, Clock: r.clk, Tick: time.Minute, AllowLocalCloneURLs: true, Secrets: r.secrets}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- r.d.Run(ctx) }()
	select {
	case <-r.d.Ready():
	case err := <-r.done:
		t.Fatalf("daemon failed to start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *rig) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("daemon exit: %v", err)
		}
		r.done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func (r *rig) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *rig) repos(t *testing.T) []store.Repo {
	t.Helper()
	rs, err := r.d.Store.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestDaemon_SyncsRepoOnScheduleAndRecordsState(t *testing.T) {
	e := gitxtest.New(t)
	r := start(t, e, cfgFor(e.Work))
	r.waitFor(t, "first sync", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].LastStatus == "ok" })

	rs := r.repos(t)
	if rs[0].DefaultBranch != "main" || rs[0].Host != "local" || rs[0].Digest == "" || rs[0].Failures != 0 {
		t.Fatalf("repo=%+v", rs[0])
	}
	first := rs[0].LastSync

	// a teammate pushes; after the interval elapses the daemon fast-forwards main
	other := e.Clone("t1")
	want := e.Commit(other, "x.txt", "x", "x")
	e.Git(other, "push", "-q", "origin", "main")
	r.advance(45 * time.Minute)
	r.waitFor(t, "second sync", func() bool { rs := r.repos(t); return rs[0].LastSync.After(first) })
	if got := e.Git(e.Work, "rev-parse", "main"); got != want {
		t.Fatalf("main=%s want %s", got, want)
	}
	if rs := r.repos(t); rs[0].LastFF != "fast-forwarded" {
		t.Fatalf("repo=%+v", rs[0])
	}
}

func TestDaemon_SecondInstanceRefused(t *testing.T) {
	e := gitxtest.New(t)
	r := start(t, e, cfgFor(e.Work))
	_ = r.d.Ready()
	d2 := &Daemon{ConfigPath: r.cfgPath, StateDir: r.d.StateDir, Runner: e.R}
	if err := d2.Run(t.Context()); !errors.Is(err, instance.ErrRunning) {
		t.Fatalf("err=%v", err)
	}
}

func TestDaemon_UnreachableRemote_FailsThenFlagsNothingYet(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "remote", "set-url", "origin", filepath.Join(e.Root, "missing.git"))
	r := start(t, e, cfgFor(e.Work))
	r.waitFor(t, "failure recorded", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].LastStatus == "failed" })
	rs := r.repos(t)
	if rs[0].Failures != 1 || rs[0].NeedsAttention || rs[0].LastReason != "ls-remote" || !rs[0].NextSync.After(r.clk.Now()) {
		t.Fatalf("repo=%+v", rs[0])
	}
}

func TestDaemon_HostileRepoFlaggedForUser(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "config", "--local", "core.sshCommand", "touch "+filepath.Join(e.Root, "PWNED"))
	r := start(t, e, cfgFor(e.Work))
	r.waitFor(t, "attention", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].NeedsAttention })
	if _, err := os.Stat(filepath.Join(e.Root, "PWNED")); err == nil {
		t.Fatal("hostile config executed")
	}
}

func TestDaemon_ConfigReload_AddsRepoAndRejectsInvalid(t *testing.T) {
	e := gitxtest.New(t)
	e2 := e.Clone("second")
	r := start(t, e, cfgFor(e.Work))
	r.waitFor(t, "first sync", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].LastStatus == "ok" })

	// invalid change is rejected and reported, previous config stays
	writeCfg(t, r.cfgPath, "[general]\ninterval = \"1m\"")
	r.advance(31 * time.Second)
	r.waitFor(t, "config-invalid event", func() bool {
		ev, _ := r.d.Store.RecentEvents(context.Background(), 20)
		for _, x := range ev {
			if x.Code == "config-invalid" {
				return true
			}
		}
		return false
	})
	if len(r.repos(t)) != 1 {
		t.Fatal("invalid config must not change the repo set")
	}

	// valid change adds the second repo (mtime/size must differ)
	writeCfg(t, r.cfgPath, cfgFor(e.Work, e2)+"# changed\n")
	r.advance(31 * time.Second)
	r.waitFor(t, "second repo tracked", func() bool { return len(r.repos(t)) == 2 })
	r.advance(time.Minute)
	r.waitFor(t, "second repo synced", func() bool {
		for _, x := range r.repos(t) {
			if x.Path == e2 && x.LastStatus == "ok" {
				return true
			}
		}
		return false
	})
}

func TestDaemon_MissingConfig_HelpfulError(t *testing.T) {
	e := gitxtest.New(t)
	d := &Daemon{ConfigPath: filepath.Join(t.TempDir(), "nope.toml"), StateDir: t.TempDir(), Runner: e.R}
	if err := d.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "repo-keeper init") {
		t.Fatalf("err=%v", err)
	}
}

func FuzzHostOf_NeverPanics_AndNeverEmpty(f *testing.F) {
	for _, s := range []string{"https://github.com/o/r", "git@host:o/r", "ssh://u@h:22/x", "", "C:\\x", "::", "https://%zz", "\x00@:"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if hostOf(s) == "" {
			t.Fatalf("empty host for %q (must fall back to \"local\")", s)
		}
	})
}

// advance moves the fake clock and then nudges every loop explicitly. Waiting for "a timer is registered" is not
// reliable with a fake clock (abandoned timers stay registered), so the loops are woken against the new time.
func (r *rig) advance(d time.Duration) {
	r.clk.Advance(d)
	r.d.Sched.Wake()
	for _, ch := range []chan struct{}{r.d.reloadWake, r.d.discWake} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
