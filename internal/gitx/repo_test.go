// SPDX-License-Identifier: Apache-2.0

package gitx_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
)

func TestLsRemote_DefaultBranchAndDigest(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	st, err := g.LsRemote(t.Context(), "origin")
	if err != nil || st.DefaultBranch != "main" || st.Digest == "" {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	e.Commit(e.Work, "a.txt", "a", "a")
	e.Git(e.Work, "push", "-q")
	st2, _ := g.LsRemote(t.Context(), "origin")
	if st2.Digest == st.Digest {
		t.Fatal("digest should change after push")
	}
	st3, _ := g.LsRemote(t.Context(), "origin")
	if st3.Digest != st2.Digest {
		t.Fatal("digest should be stable")
	}
}

func TestLsRemote_DetectsRenamedDefault(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "branch", "-m", "main", "trunk")
	e.Git(e.Work, "push", "-q", "origin", "trunk")
	e.Git(e.Origin, "symbolic-ref", "HEAD", "refs/heads/trunk")
	st, err := e.Repo(e.Work).LsRemote(t.Context(), "origin")
	if err != nil || st.DefaultBranch != "trunk" {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

func TestOriginHead_SetAndGet(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	if h, _ := g.OriginHead(t.Context(), "origin"); h != "main" {
		t.Fatalf("head=%q", h)
	}
	e.Git(e.Work, "remote", "set-head", "origin", "-d")
	if h, _ := g.OriginHead(t.Context(), "origin"); h != "" {
		t.Fatalf("head=%q after delete", h)
	}
}

func TestDirty_TrackedVsUntracked(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	if d, _ := g.Dirty(t.Context(), true); d {
		t.Fatal("fresh clone is clean")
	}
	_ = os.WriteFile(filepath.Join(e.Work, "new.txt"), []byte("x"), 0o600)
	if d, _ := g.Dirty(t.Context(), false); d {
		t.Fatal("untracked must not count when excluded")
	}
	if d, _ := g.Dirty(t.Context(), true); !d {
		t.Fatal("untracked must count when included")
	}
	_ = os.WriteFile(filepath.Join(e.Work, "README.md"), []byte("changed"), 0o600)
	if d, _ := g.Dirty(t.Context(), false); !d {
		t.Fatal("tracked change must count")
	}
}

func TestInProgress_Markers(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	if op, _ := g.InProgress(t.Context()); op != "" {
		t.Fatalf("op=%q", op)
	}
	_ = os.WriteFile(filepath.Join(e.Work, ".git", "MERGE_HEAD"), []byte("x"), 0o600)
	if op, _ := g.InProgress(t.Context()); op != "merge" {
		t.Fatalf("op=%q", op)
	}
}

func TestCurrentBranch_NamedAndDetached(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	if n, d, _ := g.CurrentBranch(t.Context()); n != "main" || d {
		t.Fatalf("n=%q d=%v", n, d)
	}
	e.Git(e.Work, "checkout", "-q", "--detach")
	if n, d, _ := g.CurrentBranch(t.Context()); n != "" || !d {
		t.Fatalf("n=%q d=%v", n, d)
	}
}

func TestBranches_UpstreamGoneAndWorktrees(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f.txt", "f", "f")
	e.Git(e.Work, "push", "-q", "-u", "origin", "feat")
	e.Git(e.Work, "checkout", "-q", "main")
	e.Git(e.Origin, "branch", "-D", "feat")
	e.Git(e.Work, "fetch", "-q", "--prune")

	bs, err := g.Branches(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]gitx.Branch{}
	for _, b := range bs {
		got[b.Name] = b
	}
	if !got["feat"].UpstreamGone || got["feat"].Upstream != "origin/feat" || got["main"].UpstreamGone || got["feat"].TipTime.IsZero() {
		t.Fatalf("branches=%+v", got)
	}

	wt := filepath.Join(e.Root, "wt")
	e.Git(e.Work, "worktree", "add", "-q", wt, "feat")
	w, _ := g.Worktrees(t.Context())
	same := func(a, b string) bool { // macOS reports /private/var/... for a /var/... temp dir
		ra, err1 := filepath.EvalSymlinks(a)
		rb, err2 := filepath.EvalSymlinks(b)
		return err1 == nil && err2 == nil && ra == rb
	}
	if w["main"] == "" || !same(w["feat"], wt) {
		t.Fatalf("worktrees=%v", w)
	}
}

func TestUniqueCommits_And_MergedInto(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f.txt", "f", "f")
	e.Git(e.Work, "checkout", "-q", "main")

	if n, err := g.UniqueCommits(t.Context(), "feat"); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if m, _ := g.MergedInto(t.Context(), "main"); m["feat"] {
		t.Fatal("feat is not merged yet")
	}
	e.Git(e.Work, "merge", "-q", "--no-ff", "-m", "merge", "feat")
	if n, _ := g.UniqueCommits(t.Context(), "feat"); n != 0 {
		t.Fatalf("n=%d after merge", n)
	}
	if m, _ := g.MergedInto(t.Context(), "main"); !m["feat"] {
		t.Fatal("feat should be merged")
	}
}

func TestPatchEquivalent_SingleCommitSquash(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f.txt", "f", "f")
	e.Git(e.Work, "checkout", "-q", "main")
	if eq, _ := g.PatchEquivalent(t.Context(), "main", "feat"); eq {
		t.Fatal("not yet on main")
	}
	e.Git(e.Work, "merge", "-q", "--squash", "feat")
	e.Git(e.Work, "commit", "-q", "-m", "squashed")
	if eq, err := g.PatchEquivalent(t.Context(), "main", "feat"); err != nil || !eq {
		t.Fatalf("eq=%v err=%v", eq, err)
	}
}

func TestTrash_PutListDrop_RoundTrip(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	sha, _, _ := g.RevParse(t.Context(), "main")
	at := time.Unix(1_700_000_000, 0)
	if _, err := g.TrashPut(t.Context(), "feature/x", sha, at); err != nil {
		t.Fatal(err)
	}
	l, err := g.TrashList(t.Context())
	if err != nil || len(l) != 1 || l[0].Branch != "feature/x" || l[0].SHA != sha || !l[0].DeletedAt.Equal(at) {
		t.Fatalf("list=%+v err=%v", l, err)
	}
	if err := g.TrashDrop(t.Context(), l[0]); err != nil {
		t.Fatal(err)
	}
	if l, _ := g.TrashList(t.Context()); len(l) != 0 {
		t.Fatalf("still listed: %+v", l)
	}
}

func TestDeleteRef_RefusesWhenTipMoved(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	old, _, _ := g.RevParse(t.Context(), "main")
	e.Git(e.Work, "branch", "tmp")
	e.Git(e.Work, "checkout", "-q", "tmp")
	e.Commit(e.Work, "x", "x", "x")
	if err := g.DeleteRef(t.Context(), "refs/heads/tmp", old); err == nil {
		t.Fatal("delete with stale sha must fail")
	}
}

func TestCheckSafeConfig_RejectsDangerousKeys(t *testing.T) {
	for _, kv := range [][2]string{
		{"core.sshCommand", "touch /tmp/pwn"}, {"core.fsmonitor", "evil"}, {"core.hooksPath", "/x"},
		{"diff.external", "evil"}, {"filter.x.smudge", "evil"}, {"url.http://evil/.insteadOf", "https://github.com/"},
		{"credential.helper", "!evil"},
	} {
		e := gitxtest.New(t)
		e.Git(e.Work, "config", "--local", kv[0], kv[1])
		if err := e.Repo(e.Work).CheckSafeConfig(t.Context()); !errors.Is(err, gitx.ErrUnsafeConfig) {
			t.Errorf("%s: err=%v", kv[0], err)
		}
	}
	e := gitxtest.New(t)
	if err := e.Repo(e.Work).CheckSafeConfig(t.Context()); err != nil {
		t.Fatalf("normal repo rejected: %v", err)
	}
}

func TestFetch_PrunesAndAllBranches(t *testing.T) {
	e := gitxtest.New(t)
	other := e.Clone("other")
	e.Git(other, "checkout", "-q", "-b", "topic")
	e.Commit(other, "t.txt", "t", "t")
	e.Git(other, "push", "-q", "origin", "topic")

	g := e.Repo(e.Work)
	if err := g.Fetch(t.Context(), "origin", false, "main"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := g.RevParse(t.Context(), "refs/remotes/origin/topic"); ok {
		t.Fatal("default-only fetch must not fetch topic")
	}
	if err := g.Fetch(t.Context(), "origin", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := g.RevParse(t.Context(), "refs/remotes/origin/topic"); !ok {
		t.Fatal("all-branch fetch must fetch topic")
	}
	e.Git(e.Origin, "branch", "-D", "topic")
	_ = g.Fetch(t.Context(), "origin", true, "")
	if _, ok, _ := g.RevParse(t.Context(), "refs/remotes/origin/topic"); ok {
		t.Fatal("prune must drop topic")
	}
}

func TestLock_ExclusiveAndReleasable(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	unlock, err := g.Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Lock(t.Context()); !errors.Is(err, gitx.ErrLocked) {
		t.Fatalf("second lock err=%v", err)
	}
	unlock()
	unlock2, err := g.Lock(t.Context())
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	unlock2()
}

func TestLock_StaleLockIsBroken(t *testing.T) {
	e := gitxtest.New(t)
	g := e.Repo(e.Work)
	p := filepath.Join(e.Work, ".git", "repo-keeper.lock")
	_ = os.WriteFile(p, []byte("999999\n"), 0o600)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(p, old, old)
	unlock, err := g.Lock(t.Context())
	if err != nil {
		t.Fatalf("stale lock should be broken: %v", err)
	}
	unlock()
}

func TestCheckSafeConfig_StandardLFSAccepted_ImpostorsRejected(t *testing.T) {
	e := gitxtest.New(t)
	for k, v := range map[string]string{
		"filter.lfs.clean": "git-lfs clean -- %f", "filter.lfs.smudge": "git-lfs smudge -- %f",
		"filter.lfs.process": "git-lfs filter-process", "filter.lfs.required": "true",
	} {
		e.Git(e.Work, "config", "--local", k, v)
	}
	if err := e.Repo(e.Work).CheckSafeConfig(t.Context()); err != nil {
		t.Fatalf("a repo set up with `git lfs install --local` must be accepted: %v", err)
	}
	for _, bad := range []struct{ k, v string }{
		{"filter.lfs.smudge", "git-lfs smudge -- %f; touch /tmp/x"},
		{"filter.lfs.process", "sh -c evil"},
		{"filter.evil.smudge", "git-lfs smudge -- %f"}, // right command, wrong filter name
		{"filter.lfs.clean", "/tmp/git-lfs clean -- %f"},
	} {
		e2 := gitxtest.New(t)
		e2.Git(e2.Work, "config", "--local", bad.k, bad.v)
		if err := e2.Repo(e2.Work).CheckSafeConfig(t.Context()); !errors.Is(err, gitx.ErrUnsafeConfig) {
			t.Errorf("%s=%q must be rejected (err=%v)", bad.k, bad.v, err)
		}
	}
}
