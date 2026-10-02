// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
)

func newApp(e *gitxtest.Env) (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &app{
		out: &out, err: &errb,
		newRunner: func() (*gitx.Runner, error) { return e.R, nil },
		clock:     clock.NewFake(time.Now().Add(time.Hour)),
	}, &out, &errb
}

func TestRun_Basics_ExitCodesAndOutput(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		code    int
		wantOut string
		wantErr string
	}{
		{"version", []string{"version"}, 0, "repo-keeper dev", ""},
		{"help", []string{"help"}, 0, "Usage:", ""},
		{"no args", nil, 2, "", "Usage:"},
		{"unknown", []string{"nope"}, 2, "", "unknown command"},
		{"sync without path", []string{"sync"}, 2, "", "usage: repo-keeper sync"},
		{"sync bad mode", []string{"sync", "--cleanup=yolo", "/x"}, 2, "", "invalid --cleanup"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := gitxtest.New(t)
			a, out, errb := newApp(e)
			if got := a.run(context.Background(), tc.args); got != tc.code {
				t.Fatalf("code=%d want %d (stderr %q)", got, tc.code, errb)
			}
			if !strings.Contains(out.String(), tc.wantOut) || !strings.Contains(errb.String(), tc.wantErr) {
				t.Fatalf("stdout=%q stderr=%q", out, errb)
			}
		})
	}
}

func TestCLI_EndToEnd_SyncCleanupRestoreAudit(t *testing.T) {
	e := gitxtest.New(t)
	journal := filepath.Join(e.Root, "audit.jsonl")
	ctx := context.Background()

	// feat is merged on origin, its remote branch deleted, a teammate also pushed to main.
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f.txt", "f", "f")
	e.Git(e.Work, "push", "-q", "-u", "origin", "feat")
	e.Git(e.Work, "checkout", "-q", "main")
	e.Git(e.Work, "merge", "-q", "--no-ff", "-m", "m", "feat")
	e.Git(e.Work, "push", "-q", "origin", "main")
	e.Git(e.Origin, "branch", "-D", "feat")

	a, out, errb := newApp(e)
	if code := a.run(ctx, []string{"sync", "--cleanup=dry-run", "--min-age=0", e.Work}); code != 0 {
		t.Fatalf("sync dry-run code=%d err=%s", code, errb)
	}
	if !strings.Contains(out.String(), "would-delete") {
		t.Fatalf("dry-run output: %s", out)
	}
	if _, err := e.R.Run(ctx, e.Work, "rev-parse", "--verify", "-q", "feat"); err != nil {
		t.Fatal("dry-run must not delete")
	}

	out.Reset()
	if code := a.run(ctx, []string{"cleanup", "--cleanup=auto", "--min-age=0", "--journal", journal, e.Work}); code != 0 {
		t.Fatalf("cleanup code=%d err=%s", code, errb)
	}
	if !strings.Contains(out.String(), "deleted") {
		t.Fatalf("cleanup output: %s", out)
	}
	if _, err := e.R.Run(ctx, e.Work, "rev-parse", "--verify", "-q", "feat"); err == nil {
		t.Fatal("feat should be deleted")
	}

	out.Reset()
	if code := a.run(ctx, []string{"audit", "--journal", journal}); code != 0 || !strings.Contains(out.String(), "deleted") {
		t.Fatalf("audit code=%d out=%s", code, out)
	}
	out.Reset()
	if code := a.run(ctx, []string{"restore", "--journal", journal, e.Work, "feat"}); code != 0 || !strings.Contains(out.String(), "restored feat") {
		t.Fatalf("restore code=%d out=%s err=%s", code, out, errb)
	}
	if _, err := e.R.Run(ctx, e.Work, "rev-parse", "--verify", "-q", "feat"); err != nil {
		t.Fatal("feat should be back")
	}
}

func TestCLI_Doctor_ReportsGitAndStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	e := gitxtest.New(t)
	a, out, errb := newApp(e)
	if code := a.run(context.Background(), []string{"doctor"}); code != 0 || !strings.Contains(out.String(), "ok   git") || !strings.Contains(out.String(), "state dir") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
}

func TestCLI_SyncAuto_WritesJournalAndRestoreUnknownFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	e := gitxtest.New(t)
	a, out, errb := newApp(e)
	ctx := context.Background()
	if code := a.run(ctx, []string{"sync", "--cleanup=auto", e.Work}); code != 0 || !strings.Contains(out.String(), "ok") {
		t.Fatalf("sync code=%d out=%s err=%s", code, out, errb)
	}
	if code := a.run(ctx, []string{"restore", e.Work, "ghost"}); code != 1 || !strings.Contains(errb.String(), "no trashed branch") {
		t.Fatalf("restore code=%d err=%s", code, errb)
	}
}

func TestCLI_SyncFailure_ExitsNonZero(t *testing.T) {
	e := gitxtest.New(t)
	a, out, _ := newApp(e)
	e.Git(e.Work, "remote", "set-url", "origin", filepath.Join(e.Root, "missing.git"))
	if code := a.run(context.Background(), []string{"sync", e.Work}); code != 1 || !strings.Contains(out.String(), "failed") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestCLI_Init_Validate_Status(t *testing.T) {
	e := gitxtest.New(t)
	a, out, errb := newApp(e)
	ctx := context.Background()
	cfg := filepath.Join(e.Root, "cfg", "config.toml")
	state := filepath.Join(e.Root, "state")

	if code := a.run(ctx, []string{"status", "--state-dir", state}); code != 1 || !strings.Contains(errb.String(), "no state yet") {
		t.Fatalf("status before daemon: code=%d err=%s", code, errb)
	}
	if code := a.run(ctx, []string{"init", "--config", cfg}); code != 0 {
		t.Fatalf("init code=%d err=%s", code, errb)
	}
	errb.Reset()
	if code := a.run(ctx, []string{"init", "--config", cfg}); code != 1 || !strings.Contains(errb.String(), "not overwriting") {
		t.Fatalf("second init must refuse: code=%d err=%s", code, errb)
	}
	out.Reset()
	if code := a.run(ctx, []string{"config", "validate", "--config", cfg}); code != 0 || !strings.Contains(out.String(), "valid") {
		t.Fatalf("validate code=%d out=%s", code, out)
	}
	_ = os.WriteFile(cfg, []byte("[general]\ninterval = \"1m\""), 0o600)
	errb.Reset()
	if code := a.run(ctx, []string{"config", "validate", "--config", cfg}); code != 1 || !strings.Contains(errb.String(), "minimum") {
		t.Fatalf("invalid validate code=%d err=%s", code, errb)
	}
	if code := a.run(ctx, []string{"config"}); code != 2 {
		t.Fatalf("config without subcommand code=%d", code)
	}
}

func TestCLI_Daemon_RunsAndStatusShowsRepo(t *testing.T) {
	e := gitxtest.New(t)
	a, out, errb := newApp(e)
	cfg := filepath.Join(e.Root, "config.toml")
	state := filepath.Join(e.Root, "state")
	_ = os.WriteFile(cfg, []byte("[cleanup]\nmode = \"off\"\n[[repo]]\npath = \""+e.Work+"\"\n"), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- a.run(ctx, []string{"daemon", "--config", cfg, "--state-dir", state, "--log-level", "error"})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out.Reset()
		if a2, o2, _ := newApp(e); a2.run(context.Background(), []string{"status", "--state-dir", state}) == 0 && strings.Contains(o2.String(), "ok") {
			out.WriteString(o2.String())
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("daemon exit=%d err=%s", code, errb)
	}
	if !strings.Contains(out.String(), e.Work) || !strings.Contains(out.String(), "main") {
		t.Fatalf("status output: %q", out.String())
	}
}
