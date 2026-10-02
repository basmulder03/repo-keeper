// SPDX-License-Identifier: Apache-2.0

package cleanup_test

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
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
)

type rig struct {
	e   *gitxtest.Env
	g   *gitx.Repo
	j   *audit.Mem
	clk *clock.Fake
}

func newRig(t *testing.T) *rig {
	e := gitxtest.New(t)
	return &rig{e: e, g: e.Repo(e.Work), j: &audit.Mem{}, clk: clock.NewFake(time.Now().Add(time.Hour))}
}

// pushedFeature creates feature branch `name` with one commit, pushed with upstream, back on main.
func (r *rig) pushedFeature(name string) {
	e := r.e
	e.Git(e.Work, "checkout", "-q", "-b", name)
	e.Commit(e.Work, name+".txt", name, "work on "+name)
	e.Git(e.Work, "push", "-q", "-u", "origin", name)
	e.Git(e.Work, "checkout", "-q", "main")
}

// mergeAndPrune merges via the given strategy on main, pushes, deletes the remote branch, prunes.
func (r *rig) mergeAndPrune(name, strategy string) {
	e := r.e
	switch strategy {
	case "merge":
		e.Git(e.Work, "merge", "-q", "--no-ff", "-m", "merge "+name, name)
	case "squash":
		e.Git(e.Work, "merge", "-q", "--squash", name)
		e.Git(e.Work, "commit", "-q", "-m", "squash "+name)
	}
	e.Git(e.Work, "push", "-q", "origin", "main")
	e.Git(e.Origin, "branch", "-D", name)
	e.Git(e.Work, "fetch", "-q", "--prune")
}

func (r *rig) run(t *testing.T, mode cleanup.Mode, mod func(*cleanup.Input)) cleanup.Report {
	in := cleanup.Input{
		Git: r.g, Repo: r.e.Work, DefaultBranch: "main", Journal: r.j, Clock: r.clk,
		Policy: cleanup.Policy{Mode: mode, Protected: cleanup.DefaultProtected},
	}
	if mod != nil {
		mod(&in)
	}
	rep, err := cleanup.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func (r *rig) exists(branch string) bool {
	_, ok, _ := r.g.RevParse(context.Background(), "refs/heads/"+branch)
	return ok
}

func outcome(rep cleanup.Report, branch string) cleanup.Item {
	for _, it := range rep.Items {
		if it.Branch == branch {
			return it
		}
	}
	return cleanup.Item{}
}

func TestRun_MergedBranch_DeletedWithTrashAndJournal(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	tip, _, _ := r.g.RevParse(t.Context(), "feat")
	r.mergeAndPrune("feat", "merge")

	rep := r.run(t, cleanup.ModeAuto, nil)

	if it := outcome(rep, "feat"); it.Outcome != cleanup.Deleted || it.Reason != cleanup.DeleteMerged {
		t.Fatalf("item=%+v", it)
	}
	if r.exists("feat") || !r.exists("main") {
		t.Fatal("feat should be gone, main kept")
	}
	trash, _ := r.g.TrashList(t.Context())
	if len(trash) != 1 || trash[0].SHA != tip || trash[0].Branch != "feat" {
		t.Fatalf("trash=%+v", trash)
	}
	if len(r.j.Entries) != 1 || r.j.Entries[0].Action != audit.Deleted || r.j.Entries[0].SHA != tip {
		t.Fatalf("journal=%+v", r.j.Entries)
	}
}

func TestRun_DryRun_ReportsButDoesNotDelete(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "merge")
	rep := r.run(t, cleanup.ModeDryRun, nil)
	if outcome(rep, "feat").Outcome != cleanup.WouldDelete || !r.exists("feat") || len(r.j.Entries) != 0 {
		t.Fatalf("rep=%+v exists=%v journal=%v", rep, r.exists("feat"), r.j.Entries)
	}
}

func TestRun_Off_DoesNothing(t *testing.T) {
	r := newRig(t)
	if rep := r.run(t, cleanup.ModeOff, nil); len(rep.Items) != 0 {
		t.Fatalf("rep=%+v", rep)
	}
}

func TestRun_SquashMerged_DeletedViaPatchEquivalence(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("sq")
	r.mergeAndPrune("sq", "squash")
	rep := r.run(t, cleanup.ModeAuto, nil)
	if it := outcome(rep, "sq"); it.Outcome != cleanup.Deleted || it.Reason != cleanup.DeletePatchEquiv {
		t.Fatalf("item=%+v", it)
	}
}

func TestRun_UnmergedBranch_IsKept(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("wip") // upstream still exists, not merged
	rep := r.run(t, cleanup.ModeAuto, nil)
	if it := outcome(rep, "wip"); it.Outcome != cleanup.Skipped || it.Reason != cleanup.SkipNotMerged || !r.exists("wip") {
		t.Fatalf("item=%+v", it)
	}
}

func TestRun_UnpushedWorkAfterMerge_IsKept(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "squash")
	// extra local commit made after the squash-merge: patch-equivalence must fail
	r.e.Git(r.e.Work, "checkout", "-q", "feat")
	r.e.Commit(r.e.Work, "more.txt", "more", "late work")
	r.e.Git(r.e.Work, "checkout", "-q", "main")
	rep := r.run(t, cleanup.ModeAuto, nil)
	if outcome(rep, "feat").Outcome != cleanup.Skipped || !r.exists("feat") {
		t.Fatalf("item=%+v", outcome(rep, "feat"))
	}
}

func TestRun_NeverPushedMerged_NeedsOptIn(t *testing.T) {
	r := newRig(t)
	r.e.Git(r.e.Work, "checkout", "-q", "-b", "local")
	r.e.Commit(r.e.Work, "l.txt", "l", "l")
	r.e.Git(r.e.Work, "checkout", "-q", "main")
	r.e.Git(r.e.Work, "merge", "-q", "--no-ff", "-m", "m", "local")

	rep := r.run(t, cleanup.ModeAuto, nil)
	if it := outcome(rep, "local"); it.Reason != cleanup.SkipNoUpstream || !r.exists("local") {
		t.Fatalf("item=%+v", it)
	}
	rep = r.run(t, cleanup.ModeAuto, func(in *cleanup.Input) { in.Policy.AllowNeverPushed = true })
	if outcome(rep, "local").Outcome != cleanup.Deleted || r.exists("local") {
		t.Fatalf("opt-in should delete: %+v", rep)
	}
}

func TestRun_FreshEmptyBranch_IsKept(t *testing.T) {
	r := newRig(t)
	r.e.Git(r.e.Work, "branch", "fresh") // same tip as main: trivially "merged"
	rep := r.run(t, cleanup.ModeAuto, nil)
	if outcome(rep, "fresh").Outcome == cleanup.Deleted || !r.exists("fresh") {
		t.Fatal("fresh empty branch must survive")
	}
}

func TestRun_DirtyTree_BlocksAndJournals(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "merge")
	_ = os.WriteFile(filepath.Join(r.e.Work, "untracked.txt"), []byte("x"), 0o600)
	rep := r.run(t, cleanup.ModeAuto, nil)
	if rep.Blocked != "dirty-working-tree" || !r.exists("feat") {
		t.Fatalf("rep=%+v", rep)
	}
	if len(r.j.Entries) != 1 || r.j.Entries[0].Action != audit.Blocked {
		t.Fatalf("journal=%+v", r.j.Entries)
	}
}

func TestRun_InProgressOp_Blocks(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "merge")
	_ = os.WriteFile(filepath.Join(r.e.Work, ".git", "MERGE_HEAD"), []byte("x"), 0o600)
	// MERGE_HEAD also makes status report nothing dirty here; the in-progress check must catch it.
	rep := r.run(t, cleanup.ModeAuto, nil)
	if rep.Blocked == "" || !r.exists("feat") {
		t.Fatalf("rep=%+v", rep)
	}
}

func TestRun_CheckedOutInWorktree_IsKept(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "merge")
	r.e.Git(r.e.Work, "worktree", "add", "-q", filepath.Join(r.e.Root, "wt"), "feat")
	rep := r.run(t, cleanup.ModeAuto, nil)
	if outcome(rep, "feat").Reason != cleanup.SkipCheckedOut || !r.exists("feat") {
		t.Fatalf("item=%+v", outcome(rep, "feat"))
	}
}

func TestRun_ProtectedAndMinAge_AreKept(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("release/1.0")
	r.mergeAndPrune("release/1.0", "merge")
	r.pushedFeature("young")
	r.mergeAndPrune("young", "merge")

	rep := r.run(t, cleanup.ModeAuto, func(in *cleanup.Input) {
		in.Policy.MinAge = 24 * time.Hour * 365 // everything is "young"
		in.Policy.Protected = append([]string{}, cleanup.DefaultProtected...)
	})
	if outcome(rep, "release/1.0").Reason != cleanup.SkipProtected || outcome(rep, "young").Reason != cleanup.SkipTooYoung {
		t.Fatalf("rep=%+v", rep.Items)
	}
}

func TestRun_JournalFailure_FailsClosed(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "merge")
	r.j.Err = errors.New("disk full")
	rep := r.run(t, cleanup.ModeAuto, nil)
	if outcome(rep, "feat").Outcome != cleanup.Failed || !r.exists("feat") {
		t.Fatalf("item=%+v", outcome(rep, "feat"))
	}
}

type movingGit struct {
	cleanup.Git
	branch, fake string
}

func (m movingGit) RevParse(ctx context.Context, rev string) (string, bool, error) {
	if rev == "refs/heads/"+m.branch {
		return m.fake, true, nil // simulates the tip moving between evaluation and deletion
	}
	return m.Git.RevParse(ctx, rev)
}

func TestRun_BranchMovedMidFlight_IsKept(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "merge")
	// RevParse also feeds the base-ref probe, which uses different refs, so only the re-verify sees the fake.
	rep := r.run(t, cleanup.ModeAuto, func(in *cleanup.Input) {
		in.Git = movingGit{Git: r.g, branch: "feat", fake: "0000000000000000000000000000000000000001"}
	})
	if outcome(rep, "feat").Outcome != cleanup.Skipped || !r.exists("feat") {
		t.Fatalf("item=%+v", outcome(rep, "feat"))
	}
}

func TestRestore_And_PurgeTrash(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	tip, _, _ := r.g.RevParse(t.Context(), "feat")
	r.mergeAndPrune("feat", "merge")
	r.run(t, cleanup.ModeAuto, nil)
	if r.exists("feat") {
		t.Fatal("setup: feat should be deleted")
	}

	if _, err := cleanup.Restore(t.Context(), r.g, r.j, r.e.Work, "feat", r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := r.g.RevParse(t.Context(), "feat"); got != tip {
		t.Fatalf("restored tip=%s want %s", got, tip)
	}
	if _, err := cleanup.Restore(t.Context(), r.g, r.j, r.e.Work, "feat", r.clk.Now()); err == nil {
		t.Fatal("restore must not overwrite an existing branch")
	}
	if _, err := cleanup.Restore(t.Context(), r.g, r.j, r.e.Work, "nope", r.clk.Now()); err == nil {
		t.Fatal("restore of unknown branch must fail")
	}

	if n, _ := cleanup.PurgeTrash(t.Context(), r.g, r.j, r.e.Work, 30*24*time.Hour, r.clk.Now()); n != 0 {
		t.Fatalf("fresh trash purged: %d", n)
	}
	if n, err := cleanup.PurgeTrash(t.Context(), r.g, r.j, r.e.Work, 30*24*time.Hour, r.clk.Now().Add(31*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestRun_NoDefaultBranch_Errors(t *testing.T) {
	r := newRig(t)
	_, err := cleanup.Run(t.Context(), cleanup.Input{Git: r.g, Journal: r.j, Clock: r.clk, Policy: cleanup.Policy{Mode: cleanup.ModeAuto}})
	if err == nil {
		t.Fatal("expected error")
	}
}

// faulty wraps a Git and injects failures at chosen steps.
type faulty struct {
	cleanup.Git
	trashErr, deleteErr error
	unique              int
}

func (f faulty) TrashPut(ctx context.Context, b, sha string, at time.Time) (string, error) {
	if f.trashErr != nil {
		return "", f.trashErr
	}
	return f.Git.TrashPut(ctx, b, sha, at)
}

func (f faulty) DeleteRef(ctx context.Context, ref, sha string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.Git.DeleteRef(ctx, ref, sha)
}

func (f faulty) UniqueCommits(context.Context, string) (int, error) { return f.unique, nil }

func TestRun_FailureInjection_NeverDeletesOnDoubt(t *testing.T) {
	tests := []struct {
		name        string
		g           func(cleanup.Git) cleanup.Git
		wantOutcome cleanup.Outcome
		wantJournal audit.Action
	}{
		{"trash fails", func(g cleanup.Git) cleanup.Git { return faulty{Git: g, trashErr: errors.New("x")} }, cleanup.Failed, ""},
		{"delete fails", func(g cleanup.Git) cleanup.Git { return faulty{Git: g, deleteErr: errors.New("x")} }, cleanup.Failed, audit.DeleteFailed},
		{"unique commits found", func(g cleanup.Git) cleanup.Git { return faulty{Git: g, unique: 3} }, cleanup.Skipped, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.pushedFeature("feat")
			r.mergeAndPrune("feat", "merge")
			rep := r.run(t, cleanup.ModeAuto, func(in *cleanup.Input) { in.Git = tc.g(r.g) })
			if outcome(rep, "feat").Outcome != tc.wantOutcome || !r.exists("feat") {
				t.Fatalf("item=%+v", outcome(rep, "feat"))
			}
			last := audit.Action("")
			if n := len(r.j.Entries); n > 0 {
				last = r.j.Entries[n-1].Action
			}
			if last != tc.wantJournal {
				t.Fatalf("last journal action=%q want %q", last, tc.wantJournal)
			}
		})
	}
}

func TestRun_NoJournal_FailsClosed(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("feat")
	r.mergeAndPrune("feat", "merge")
	rep := r.run(t, cleanup.ModeAuto, func(in *cleanup.Input) { in.Journal = nil })
	if outcome(rep, "feat").Outcome != cleanup.Failed || !r.exists("feat") {
		t.Fatalf("item=%+v", outcome(rep, "feat"))
	}
}

func TestRun_ProviderMerged_LazyAndTipMatched(t *testing.T) {
	r := newRig(t)
	r.pushedFeature("pr-ok")
	r.pushedFeature("pr-late")
	r.pushedFeature("untouched")
	r.e.Git(r.e.Work, "push", "-q", "origin", "main")
	// two-commit branch so patch-equivalence cannot prove the squash; only the provider can
	for _, b := range []string{"pr-ok", "pr-late"} {
		r.e.Git(r.e.Work, "checkout", "-q", b)
		r.e.Commit(r.e.Work, b+"2.txt", b, b+" second")
		r.e.Git(r.e.Work, "push", "-q", "origin", b)
		r.e.Git(r.e.Work, "checkout", "-q", "main")
	}
	okTip, _, _ := r.g.RevParse(t.Context(), "pr-ok")
	lateTip, _, _ := r.g.RevParse(t.Context(), "pr-late")
	r.e.Git(r.e.Origin, "branch", "-D", "pr-ok")
	r.e.Git(r.e.Origin, "branch", "-D", "pr-late")
	r.e.Git(r.e.Work, "fetch", "-q", "--prune")

	var asked []string
	rep := r.run(t, cleanup.ModeAuto, func(in *cleanup.Input) {
		in.ProviderMerged = func(_ context.Context, names []string) (map[string]string, error) {
			asked = names
			return map[string]string{"pr-ok": okTip, "pr-late": "deadbeef" + lateTip[8:]}, nil // late: PR head != local tip
		}
	})
	if outcome(rep, "pr-ok").Reason != cleanup.DeleteProviderMerged || outcome(rep, "pr-ok").Outcome != cleanup.Deleted {
		t.Fatalf("pr-ok=%+v", outcome(rep, "pr-ok"))
	}
	if outcome(rep, "pr-late").Outcome == cleanup.Deleted || !r.exists("pr-late") {
		t.Fatal("branch with commits newer than the merged PR head must survive")
	}
	for _, n := range asked {
		if n == "untouched" {
			t.Fatal("provider must only be asked about upstream-gone branches")
		}
	}
}
