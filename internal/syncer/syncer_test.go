// SPDX-License-Identifier: Apache-2.0

package syncer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/syncer"
)

func input(e *gitxtest.Env, dir string) syncer.Input {
	return syncer.Input{
		Git: e.Repo(dir), Path: dir, AllBranches: true, Journal: &audit.Mem{},
		Clock: clock.NewFake(time.Now().Add(time.Hour)),
	}
}

// teammatePushes lands a commit on origin/main from another clone and returns its SHA.
func teammatePushes(e *gitxtest.Env, name string) string {
	other := e.Clone(name)
	sha := e.Commit(other, name+".txt", name, name)
	e.Git(other, "push", "-q", "origin", "main")
	return sha
}

func TestSync_CheckedOutClean_FastForwards(t *testing.T) {
	e := gitxtest.New(t)
	want := teammatePushes(e, "t1")
	res := syncer.Sync(t.Context(), input(e, e.Work))
	if res.Status != syncer.OK || res.FF != syncer.FastForwarded || !res.Fetched || res.DefaultBranch != "main" {
		t.Fatalf("res=%+v", res)
	}
	if got := e.Git(e.Work, "rev-parse", "main"); got != want {
		t.Fatalf("main=%s want %s", got, want)
	}
	if again := syncer.Sync(t.Context(), input(e, e.Work)); again.FF != syncer.UpToDate {
		t.Fatalf("second run: %+v", again)
	}
}

func TestSync_NotCheckedOut_UpdatesRefWithoutTouchingWorktree(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "checkout", "-q", "-b", "feature")
	e.Commit(e.Work, "f.txt", "f", "f")
	want := teammatePushes(e, "t1")
	res := syncer.Sync(t.Context(), input(e, e.Work))
	if res.FF != syncer.FastForwarded {
		t.Fatalf("res=%+v", res)
	}
	if e.Git(e.Work, "rev-parse", "main") != want || e.Git(e.Work, "branch", "--show-current") != "feature" {
		t.Fatal("main must advance while feature stays checked out")
	}
}

func TestSync_DirtyTrackedChange_SkipsFastForward(t *testing.T) {
	e := gitxtest.New(t)
	before := e.Git(e.Work, "rev-parse", "main")
	teammatePushes(e, "t1")
	_ = os.WriteFile(filepath.Join(e.Work, "README.md"), []byte("local edit"), 0o600)
	res := syncer.Sync(t.Context(), input(e, e.Work))
	if res.FF != syncer.SkippedDirty || res.Status != syncer.OK {
		t.Fatalf("res=%+v", res)
	}
	if e.Git(e.Work, "rev-parse", "main") != before {
		t.Fatal("main must not move")
	}
	if b, _ := os.ReadFile(filepath.Join(e.Work, "README.md")); string(b) != "local edit" {
		t.Fatal("local edit lost")
	}
}

func TestSync_UntrackedFile_DoesNotBlockFastForward(t *testing.T) {
	e := gitxtest.New(t)
	teammatePushes(e, "t1")
	_ = os.WriteFile(filepath.Join(e.Work, "notes.txt"), []byte("mine"), 0o600)
	if res := syncer.Sync(t.Context(), input(e, e.Work)); res.FF != syncer.FastForwarded {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_Diverged_LeavesBranchAlone(t *testing.T) {
	e := gitxtest.New(t)
	teammatePushes(e, "t1")
	mine := e.Commit(e.Work, "mine.txt", "m", "local commit")
	res := syncer.Sync(t.Context(), input(e, e.Work))
	if res.FF != syncer.Diverged || e.Git(e.Work, "rev-parse", "main") != mine {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_LocalAhead_Reported(t *testing.T) {
	e := gitxtest.New(t)
	e.Commit(e.Work, "mine.txt", "m", "local commit")
	if res := syncer.Sync(t.Context(), input(e, e.Work)); res.FF != syncer.Ahead {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_OtherWorktreeHasDefault_Skips(t *testing.T) {
	e := gitxtest.New(t)
	teammatePushes(e, "t1")
	e.Git(e.Work, "checkout", "-q", "-b", "feature")
	e.Git(e.Work, "worktree", "add", "-q", filepath.Join(e.Root, "wt"), "main")
	if res := syncer.Sync(t.Context(), input(e, e.Work)); res.FF != syncer.SkippedWorktree {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_RebaseInProgress_SkipsFastForwardButFetches(t *testing.T) {
	e := gitxtest.New(t)
	teammatePushes(e, "t1")
	_ = os.MkdirAll(filepath.Join(e.Work, ".git", "rebase-merge"), 0o750)
	res := syncer.Sync(t.Context(), input(e, e.Work))
	if res.FF != syncer.SkippedInProgress || !res.Fetched {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_DefaultBranchRenamed_DetectedAndOriginHeadUpdated(t *testing.T) {
	e := gitxtest.New(t)
	stale := e.Clone("stale") // a second machine that still thinks "main" is the default
	e.Git(e.Work, "branch", "-m", "main", "trunk")
	e.Git(e.Work, "push", "-q", "origin", "trunk")
	e.Git(e.Origin, "symbolic-ref", "HEAD", "refs/heads/trunk")

	other := e.Clone("t2")
	e.Git(other, "checkout", "-q", "trunk")
	want := e.Commit(other, "x.txt", "x", "x")
	e.Git(other, "push", "-q", "origin", "trunk")

	res := syncer.Sync(t.Context(), input(e, stale))
	if res.DefaultBranch != "trunk" || !res.DefaultChanged || res.PrevDefault != "main" {
		t.Fatalf("res=%+v", res)
	}
	if res.FF != syncer.SkippedNoLocal {
		t.Fatalf("no local trunk exists, must not be created implicitly: %+v", res)
	}
	if e.Git(stale, "symbolic-ref", "refs/remotes/origin/HEAD") != "refs/remotes/origin/trunk" {
		t.Fatal("origin/HEAD not updated")
	}
	if e.Git(stale, "rev-parse", "origin/trunk") != want {
		t.Fatal("trunk not fetched")
	}
	if _, err := os.Stat(filepath.Join(stale, ".git")); err != nil || e.Git(stale, "rev-parse", "main") == "" {
		t.Fatal("local main must be left alone")
	}
}

func TestSync_UnchangedRemote_SkipsFetch(t *testing.T) {
	e := gitxtest.New(t)
	first := syncer.Sync(t.Context(), input(e, e.Work))
	if !first.Fetched || first.Digest == "" {
		t.Fatalf("first=%+v", first)
	}
	in := input(e, e.Work)
	in.PrevDigest = first.Digest
	if second := syncer.Sync(t.Context(), in); second.Fetched || second.Status != syncer.OK {
		t.Fatalf("second=%+v", second)
	}
	teammatePushes(e, "t1")
	if third := syncer.Sync(t.Context(), in); !third.Fetched || third.FF != syncer.FastForwarded {
		t.Fatalf("third=%+v", third)
	}
}

func TestSync_UnreachableRemote_FailsWithoutDigest(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "remote", "set-url", "origin", filepath.Join(e.Root, "does-not-exist.git"))
	res := syncer.Sync(t.Context(), input(e, e.Work))
	if res.Status != syncer.Failed || res.Reason != "ls-remote" || res.Err == nil {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_LockedRepo_IsSkipped(t *testing.T) {
	e := gitxtest.New(t)
	unlock, _ := e.Repo(e.Work).Lock(t.Context())
	defer unlock()
	if res := syncer.Sync(t.Context(), input(e, e.Work)); res.Status != syncer.Skipped || res.Reason != "locked" {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_HostileRepo_RefusedAndNothingExecutes(t *testing.T) {
	e := gitxtest.New(t)
	teammatePushes(e, "t1")
	marker := filepath.Join(e.Root, "PWNED")
	e.Git(e.Work, "config", "--local", "core.sshCommand", "touch "+marker)
	if res := syncer.Sync(t.Context(), input(e, e.Work)); res.Status != syncer.Failed || res.Reason != "unsafe-config" {
		t.Fatalf("res=%+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hostile config executed")
	}
}

func TestSync_HookInRepo_NeverRuns(t *testing.T) {
	e := gitxtest.New(t)
	teammatePushes(e, "t1")
	marker := filepath.Join(e.Root, "HOOK-RAN")
	hook := filepath.Join(e.Work, ".git", "hooks", "post-merge")
	_ = os.MkdirAll(filepath.Dir(hook), 0o750)
	_ = os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700) //nolint:gosec // test fixture
	res := syncer.Sync(t.Context(), input(e, e.Work))
	if res.FF != syncer.FastForwarded {
		t.Fatalf("res=%+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("post-merge hook executed")
	}
}

func TestSync_WithCleanup_DeletesMergedBranch(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f.txt", "f", "f")
	e.Git(e.Work, "push", "-q", "-u", "origin", "feat")
	e.Git(e.Work, "checkout", "-q", "main")
	e.Git(e.Work, "merge", "-q", "--no-ff", "-m", "m", "feat")
	e.Git(e.Work, "push", "-q", "origin", "main")
	e.Git(e.Origin, "branch", "-D", "feat")

	in := input(e, e.Work)
	in.Cleanup = cleanup.Policy{Mode: cleanup.ModeAuto, Protected: cleanup.DefaultProtected}
	res := syncer.Sync(t.Context(), in)
	if res.Status != syncer.OK || res.Cleanup == nil {
		t.Fatalf("res=%+v", res)
	}
	if _, err := e.R.Run(t.Context(), e.Work, "rev-parse", "--verify", "-q", "refs/heads/feat"); err == nil {
		t.Fatal("feat should have been cleaned up after prune")
	}
}

func TestSync_DefaultOnlyFetch_SkipsOtherBranches(t *testing.T) {
	e := gitxtest.New(t)
	other := e.Clone("t1")
	e.Git(other, "checkout", "-q", "-b", "topic")
	e.Commit(other, "t.txt", "t", "t")
	e.Git(other, "push", "-q", "origin", "topic")
	in := input(e, e.Work)
	in.AllBranches = false
	if res := syncer.Sync(t.Context(), in); res.Status != syncer.OK {
		t.Fatalf("res=%+v", res)
	}
	if _, err := e.R.Run(t.Context(), e.Work, "rev-parse", "--verify", "-q", "refs/remotes/origin/topic"); err == nil {
		t.Fatal("topic must not be fetched")
	}
}

type failFetch struct{ syncer.Git }

func (failFetch) Fetch(context.Context, string, bool, string) error {
	return errors.New("network down")
}

func TestSync_FetchFails_ReportedWithoutDigest(t *testing.T) {
	e := gitxtest.New(t)
	in := input(e, e.Work)
	in.Git = failFetch{e.Repo(e.Work)}
	res := syncer.Sync(t.Context(), in)
	if res.Status != syncer.Failed || res.Reason != "fetch" || res.Digest != "" || res.DefaultBranch != "main" {
		t.Fatalf("res=%+v", res)
	}
}

func TestSync_NoLocalDefaultBranch_NotCreated(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "checkout", "-q", "-b", "other")
	e.Git(e.Work, "branch", "-D", "main")
	if res := syncer.Sync(t.Context(), input(e, e.Work)); res.FF != syncer.SkippedNoLocal {
		t.Fatalf("res=%+v", res)
	}
}
