// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrUnsafeConfig means repo-local config could make git run external commands.
var ErrUnsafeConfig = errors.New("gitx: repository config contains unsafe keys")

// Repo is a local clone operated on through a Runner.
type Repo struct {
	R   *Runner
	Dir string
}

// Repo binds dir to the runner.
func (r *Runner) Repo(dir string) *Repo { return &Repo{R: r, Dir: dir} }

func (g *Repo) run(ctx context.Context, args ...string) (string, error) {
	return g.R.Run(ctx, g.Dir, args...)
}

func (g *Repo) line(ctx context.Context, args ...string) (string, error) {
	out, err := g.run(ctx, args...)
	return strings.TrimSpace(out), err
}

// unsafeKeys are repo-local settings that can execute commands or redirect traffic; fail closed on them.
var unsafeKeyPrefixes = []string{
	"core.sshcommand", "core.fsmonitor", "core.hookspath", "core.pager", "core.editor", "core.gitproxy",
	"core.askpass", "diff.external", "credential.helper", "gpg.program", "include.path", "includeif.",
}

// CheckSafeConfig refuses repos whose local config could execute commands or redirect remotes.
func (g *Repo) CheckSafeConfig(ctx context.Context) error {
	out, err := g.run(ctx, "config", "--local", "--name-only", "--list", "-z")
	if err != nil {
		if ExitCode(err) == 1 { // no local config entries
			return nil
		}
		return err
	}
	var bad []string
	for _, k := range strings.Split(out, "\x00") {
		lk := strings.ToLower(k)
		switch {
		case lk == "":
		case strings.HasSuffix(lk, ".insteadof") || strings.HasSuffix(lk, ".pushinsteadof"):
			bad = append(bad, k)
		case strings.HasPrefix(lk, "filter.") && (strings.HasSuffix(lk, ".clean") || strings.HasSuffix(lk, ".smudge") || strings.HasSuffix(lk, ".process")):
			bad = append(bad, k)
		case strings.HasPrefix(lk, "remote.") && (strings.HasSuffix(lk, ".uploadpack") || strings.HasSuffix(lk, ".receivepack") || strings.HasSuffix(lk, ".vcs")):
			bad = append(bad, k)
		default:
			for _, p := range unsafeKeyPrefixes {
				if strings.HasPrefix(lk, p) {
					bad = append(bad, k)
					break
				}
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrUnsafeConfig, strings.Join(bad, ", "))
	}
	return nil
}

// gitDir returns the absolute .git directory (worktree-aware).
func (g *Repo) gitDir(ctx context.Context) (string, error) {
	return g.line(ctx, "rev-parse", "--absolute-git-dir")
}

// InProgress names an unfinished operation (rebase, merge, ...) or "" if none; a held index.lock also counts.
func (g *Repo) InProgress(ctx context.Context) (string, error) {
	gd, err := g.gitDir(ctx)
	if err != nil {
		return "", err
	}
	markers := []struct{ file, name string }{
		{"rebase-merge", "rebase"}, {"rebase-apply", "rebase/am"}, {"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"}, {"REVERT_HEAD", "revert"}, {"BISECT_LOG", "bisect"},
		{"index.lock", "index.lock"},
	}
	for _, m := range markers {
		if _, err := os.Lstat(filepath.Join(gd, m.file)); err == nil {
			return m.name, nil
		}
	}
	return "", nil
}

// CurrentBranch returns the checked-out branch; detached is true (and name empty) on a detached HEAD.
func (g *Repo) CurrentBranch(ctx context.Context) (name string, detached bool, err error) {
	out, err := g.line(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		if ExitCode(err) == 1 {
			return "", true, nil
		}
		return "", false, err
	}
	return strings.TrimPrefix(out, "refs/heads/"), false, nil
}

// Dirty reports uncommitted changes; untracked files count only when includeUntracked is set.
func (g *Repo) Dirty(ctx context.Context, includeUntracked bool) (bool, error) {
	u := "--untracked-files=no"
	if includeUntracked {
		u = "--untracked-files=normal"
	}
	out, err := g.run(ctx, "status", "--porcelain", "-z", u, "--ignore-submodules=all")
	return out != "", err
}

// RevParse resolves rev to a commit SHA; ok is false when it does not exist.
func (g *Repo) RevParse(ctx context.Context, rev string) (sha string, ok bool, err error) {
	out, err := g.line(ctx, "rev-parse", "--verify", "-q", rev+"^{commit}")
	if err != nil {
		if ExitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (g *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := g.run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// RemoteState is one ls-remote snapshot.
type RemoteState struct {
	// DefaultBranch is HEAD's target on the remote ("" if the remote has no HEAD symref).
	DefaultBranch string
	// Digest fingerprints all branch and tag tips; equal digests mean nothing changed.
	Digest string
}

// LsRemote queries heads, tags and the HEAD symref in a single network round-trip.
func (g *Repo) LsRemote(ctx context.Context, remote string) (RemoteState, error) {
	out, err := g.run(ctx, "ls-remote", "--symref", remote, "HEAD", "refs/heads/*", "refs/tags/*")
	if err != nil {
		return RemoteState{}, err
	}
	var st RemoteState
	var refs []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		l := sc.Text()
		if rest, ok := strings.CutPrefix(l, "ref: refs/heads/"); ok {
			if name, _, ok := strings.Cut(rest, "\t"); ok {
				st.DefaultBranch = name
			}
			continue
		}
		refs = append(refs, l)
	}
	sort.Strings(refs)
	sum := sha256.Sum256([]byte(strings.Join(refs, "\n")))
	st.Digest = hex.EncodeToString(sum[:])
	return st, nil
}

// OriginHead returns the branch origin/HEAD points to locally ("" if unset).
func (g *Repo) OriginHead(ctx context.Context, remote string) (string, error) {
	out, err := g.line(ctx, "symbolic-ref", "-q", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		if ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimPrefix(out, "refs/remotes/"+remote+"/"), nil
}

// SetOriginHead points refs/remotes/<remote>/HEAD at branch.
func (g *Repo) SetOriginHead(ctx context.Context, remote, branch string) error {
	_, err := g.run(ctx, "remote", "set-head", remote, branch)
	return err
}

// Fetch updates remote-tracking refs and prunes deleted ones; allBranches=false fetches only branch.
func (g *Repo) Fetch(ctx context.Context, remote string, allBranches bool, branch string) error {
	args := []string{"fetch", "--prune", "--no-recurse-submodules", "--no-write-fetch-head", remote}
	if !allBranches {
		args = append(args, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch))
	}
	_, err := g.run(ctx, args...)
	return err
}

// MergeFFOnly fast-forwards the checked-out branch to target; git refuses anything else.
func (g *Repo) MergeFFOnly(ctx context.Context, target string) error {
	_, err := g.run(ctx, "merge", "--ff-only", "--no-edit", "--no-verify", target)
	return err
}

// UpdateRef moves ref to sha only if it currently equals old ("" = must not exist).
func (g *Repo) UpdateRef(ctx context.Context, ref, sha, old string) error {
	if old == "" {
		old = strings.Repeat("0", len(sha))
	}
	_, err := g.run(ctx, "update-ref", ref, sha, old)
	return err
}

// DeleteRef deletes ref only if it still equals sha (race-safe).
func (g *Repo) DeleteRef(ctx context.Context, ref, sha string) error {
	_, err := g.run(ctx, "update-ref", "-d", ref, sha)
	return err
}

// Branch describes one local branch.
type Branch struct {
	Name         string
	SHA          string
	Upstream     string // short name, e.g. origin/feature; "" if none configured
	UpstreamGone bool   // upstream configured but missing on the remote
	TipTime      time.Time
}

// Branches lists local branches.
func (g *Repo) Branches(ctx context.Context) ([]Branch, error) {
	const f = "%(refname:short)%00%(objectname)%00%(upstream:short)%00%(upstream:track)%00%(committerdate:unix)"
	out, err := g.run(ctx, "for-each-ref", "--format="+f, "refs/heads")
	if err != nil {
		return nil, err
	}
	var res []Branch
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		p := strings.Split(l, "\x00")
		if len(p) != 5 {
			return nil, fmt.Errorf("gitx: unexpected for-each-ref line %q", l)
		}
		b := Branch{Name: p[0], SHA: p[1], Upstream: p[2], UpstreamGone: strings.Contains(p[3], "gone")}
		if ts, err := strconv.ParseInt(p[4], 10, 64); err == nil {
			b.TipTime = time.Unix(ts, 0)
		}
		res = append(res, b)
	}
	return res, nil
}

// Worktrees maps branch name to the worktree path that has it checked out (main worktree included).
func (g *Repo) Worktrees(ctx context.Context) (map[string]string, error) {
	out, err := g.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	res := map[string]string{}
	var path string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			path = strings.TrimPrefix(l, "worktree ")
		case strings.HasPrefix(l, "branch refs/heads/"):
			res[strings.TrimPrefix(l, "branch refs/heads/")] = path
		}
	}
	return res, nil
}

// MergedInto returns local branches whose tip is reachable from rev.
func (g *Repo) MergedInto(ctx context.Context, rev string) (map[string]bool, error) {
	out, err := g.run(ctx, "for-each-ref", "--merged="+rev, "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	res := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res[l] = true
		}
	}
	return res, nil
}

// UniqueCommits counts commits reachable only from branch (not from any other branch or remote ref).
func (g *Repo) UniqueCommits(ctx context.Context, branch string) (int, error) {
	out, err := g.line(ctx, "rev-list", "--count", "refs/heads/"+branch,
		"--not", "--exclude="+branch, "--branches", "--remotes") // --exclude is relative to refs/heads/
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// PatchEquivalent reports whether every commit of branch has a patch-equal commit in base (git cherry).
func (g *Repo) PatchEquivalent(ctx context.Context, base, branch string) (bool, error) {
	out, err := g.run(ctx, "cherry", base, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	n := 0
	for sc.Scan() {
		n++
		if strings.HasPrefix(sc.Text(), "+") {
			return false, nil
		}
	}
	return n > 0, sc.Err() // zero commits means "merged", not "squash-merged"
}

// TrashRef is a recoverable deleted branch.
type TrashRef struct {
	Branch    string
	SHA       string
	DeletedAt time.Time
	Ref       string
}

const trashPrefix = "refs/repo-keeper/trash/"

// TrashPut records sha under the trash namespace and returns its ref.
func (g *Repo) TrashPut(ctx context.Context, branch, sha string, at time.Time) (string, error) {
	ref := fmt.Sprintf("%s%d/%s", trashPrefix, at.Unix(), branch)
	// Same-second collisions for one branch are the same deletion; update-ref with no old value is idempotent.
	_, err := g.run(ctx, "update-ref", ref, sha)
	return ref, err
}

// TrashList returns trash entries, newest first.
func (g *Repo) TrashList(ctx context.Context) ([]TrashRef, error) {
	out, err := g.run(ctx, "for-each-ref", "--format=%(refname)%00%(objectname)", trashPrefix)
	if err != nil {
		return nil, err
	}
	var res []TrashRef
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		ref, sha, ok := strings.Cut(l, "\x00")
		if !ok {
			continue
		}
		ts, name, ok := strings.Cut(strings.TrimPrefix(ref, trashPrefix), "/")
		sec, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil {
			continue
		}
		res = append(res, TrashRef{Branch: name, SHA: sha, DeletedAt: time.Unix(sec, 0), Ref: ref})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].DeletedAt.After(res[j].DeletedAt) })
	return res, nil
}

// TrashDrop removes a trash ref.
func (g *Repo) TrashDrop(ctx context.Context, t TrashRef) error {
	return g.DeleteRef(ctx, t.Ref, t.SHA)
}
