// SPDX-License-Identifier: Apache-2.0

package cleanup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
)

// Git is the slice of git the cleaner needs; *gitx.Repo implements it.
type Git interface {
	Branches(ctx context.Context) ([]gitx.Branch, error)
	Worktrees(ctx context.Context) (map[string]string, error)
	CurrentBranch(ctx context.Context) (string, bool, error)
	MergedInto(ctx context.Context, rev string) (map[string]bool, error)
	RevParse(ctx context.Context, rev string) (string, bool, error)
	UniqueCommits(ctx context.Context, branch string) (int, error)
	PatchEquivalent(ctx context.Context, base, branch string) (bool, error)
	Dirty(ctx context.Context, includeUntracked bool) (bool, error)
	InProgress(ctx context.Context) (string, error)
	TrashPut(ctx context.Context, branch, sha string, at time.Time) (string, error)
	TrashList(ctx context.Context) ([]gitx.TrashRef, error)
	TrashDrop(ctx context.Context, t gitx.TrashRef) error
	DeleteRef(ctx context.Context, ref, sha string) error
	UpdateRef(ctx context.Context, ref, sha, old string) error
}

// MergedLookup returns branch -> head SHA of its merged PR for the given branches.
type MergedLookup func(ctx context.Context, branches []string) (map[string]string, error)

// Input configures one cleanup pass over one repository.
type Input struct {
	Git           Git
	Repo          string // label for the journal (usually the path)
	Remote        string // default "origin"
	DefaultBranch string
	Policy        Policy
	Journal       audit.Journal
	Clock         clock.Clock
	// ProviderMerged is queried lazily, only for upstream-gone branches git cannot prove merged.
	ProviderMerged MergedLookup
}

// Outcome is what happened to one branch.
type Outcome string

// Per-branch outcomes.
const (
	Deleted     Outcome = "deleted"
	WouldDelete Outcome = "would-delete"
	Skipped     Outcome = "skipped"
	Failed      Outcome = "failed"
)

// Item is the result for one branch.
type Item struct {
	Branch  string
	SHA     string
	Reason  Reason
	Outcome Outcome
	Err     string
}

// Report summarises a pass.
type Report struct {
	Mode  Mode
	Items []Item
	// Blocked is set when candidates existed but the repo state forbade deleting (dirty, rebase in progress...).
	Blocked string
}

// Run evaluates every local branch and, in auto mode, deletes the safe ones recoverably.
func Run(ctx context.Context, in Input) (Report, error) {
	rep := Report{Mode: in.Policy.Mode}
	if in.Policy.Mode == ModeOff || in.Policy.Mode == "" {
		rep.Mode = ModeOff
		return rep, nil
	}
	if in.DefaultBranch == "" {
		return rep, errors.New("cleanup: default branch unknown")
	}
	remote := in.Remote
	if remote == "" {
		remote = "origin"
	}
	g := in.Git

	branches, err := g.Branches(ctx)
	if err != nil {
		return rep, err
	}
	worktrees, err := g.Worktrees(ctx)
	if err != nil {
		return rep, err
	}
	current, _, err := g.CurrentBranch(ctx)
	if err != nil {
		return rep, err
	}
	merged := map[string]bool{}
	haveBase := false
	for _, rev := range []string{"refs/heads/" + in.DefaultBranch, "refs/remotes/" + remote + "/" + in.DefaultBranch} {
		if _, ok, err := g.RevParse(ctx, rev); err != nil {
			return rep, err
		} else if !ok {
			continue
		}
		haveBase = true
		m, err := g.MergedInto(ctx, rev)
		if err != nil {
			return rep, err
		}
		for k := range m {
			merged[k] = true
		}
	}
	baseRef := "refs/remotes/" + remote + "/" + in.DefaultBranch
	if _, ok, _ := g.RevParse(ctx, baseRef); !ok {
		baseRef = "refs/heads/" + in.DefaultBranch
	}

	provMerged := in.lookupMerged(ctx, branches, merged, haveBase)

	now := in.Clock.Now()
	for _, b := range branches {
		f := Facts{
			Name: b.Name, IsDefault: b.Name == in.DefaultBranch, IsCurrent: b.Name == current,
			CheckedOut: boolTri(worktrees[b.Name] != ""), TipTime: b.TipTime,
			HasUpstream: boolTri(b.Upstream != ""), UpstreamGone: boolTri(b.UpstreamGone),
		}
		if haveBase {
			f.MergedIntoDefault = boolTri(merged[b.Name])
		}
		if head, ok := provMerged[b.Name]; ok {
			f.ProviderMergedTip = boolTri(head == b.SHA)
		}
		if haveBase && !merged[b.Name] && b.UpstreamGone {
			if eq, err := g.PatchEquivalent(ctx, baseRef, b.Name); err == nil {
				f.PatchEquivalent = boolTri(eq)
			}
		}
		d := Decide(f, in.Policy, now)
		it := Item{Branch: b.Name, SHA: b.SHA, Reason: d.Reason, Outcome: Skipped}
		if d.Delete {
			it.Outcome = WouldDelete
		}
		rep.Items = append(rep.Items, it)
	}

	if in.Policy.Mode != ModeAuto || !hasOutcome(rep.Items, WouldDelete) {
		return rep, nil
	}
	if why, err := blockedReason(ctx, g); err != nil {
		return rep, err
	} else if why != "" {
		rep.Blocked = why
		return rep, in.journal(ctx, audit.Entry{Time: now, Repo: in.Repo, Action: audit.Blocked, Mode: string(in.Policy.Mode), Reason: why})
	}

	for i := range rep.Items {
		if rep.Items[i].Outcome == WouldDelete {
			in.deleteOne(ctx, &rep.Items[i], now)
		}
	}
	return rep, nil
}

// blockedReason names why deletions must not happen right now ("" = free to proceed).
func blockedReason(ctx context.Context, g Git) (string, error) {
	if d, err := g.Dirty(ctx, true); err != nil {
		return "", err
	} else if d {
		return "dirty-working-tree", nil
	}
	if op, err := g.InProgress(ctx); err != nil {
		return "", err
	} else if op != "" {
		return "in-progress:" + op, nil
	}
	return "", nil
}

// deleteOne re-verifies, trashes, journals (write-ahead), then deletes; any doubt leaves the branch alone.
func (in Input) deleteOne(ctx context.Context, it *Item, now time.Time) {
	fail := func(err error) {
		it.Outcome, it.Err = Failed, err.Error()
	}
	ref := "refs/heads/" + it.Branch
	if cur, ok, err := in.Git.RevParse(ctx, ref); err != nil || !ok || cur != it.SHA {
		it.Outcome, it.Err = Skipped, "branch moved since evaluation"
		return
	}
	if it.Reason == DeleteMerged {
		if n, err := in.Git.UniqueCommits(ctx, it.Branch); err != nil || n != 0 {
			it.Outcome, it.Err = Skipped, "branch has commits not reachable elsewhere"
			return
		}
	}
	trash, err := in.Git.TrashPut(ctx, it.Branch, it.SHA, now)
	if err != nil {
		fail(err)
		return
	}
	e := audit.Entry{Time: now, Repo: in.Repo, Branch: it.Branch, SHA: it.SHA, Action: audit.Deleted, Mode: string(in.Policy.Mode), Reason: string(it.Reason), Detail: trash}
	if err := in.journal(ctx, e); err != nil {
		fail(fmt.Errorf("journal unavailable, not deleting: %w", err))
		return
	}
	if err := in.Git.DeleteRef(ctx, ref, it.SHA); err != nil {
		e.Action, e.Detail = audit.DeleteFailed, err.Error()
		_ = in.journal(ctx, e)
		fail(err)
		return
	}
	it.Outcome = Deleted
}

func (in Input) journal(ctx context.Context, e audit.Entry) error {
	if in.Journal == nil {
		return errors.New("cleanup: no journal configured")
	}
	return in.Journal.Append(ctx, e)
}

// Restore recreates the most recently trashed branch with this name; it never overwrites an existing branch.
func Restore(ctx context.Context, g Git, j audit.Journal, repo, branch string, now time.Time) (gitx.TrashRef, error) {
	list, err := g.TrashList(ctx)
	if err != nil {
		return gitx.TrashRef{}, err
	}
	for _, t := range list { // newest first
		if t.Branch != branch {
			continue
		}
		if err := g.UpdateRef(ctx, "refs/heads/"+branch, t.SHA, ""); err != nil {
			return t, fmt.Errorf("cleanup: restore %q (does it already exist?): %w", branch, err)
		}
		if j != nil {
			if err := j.Append(ctx, audit.Entry{Time: now, Repo: repo, Branch: branch, SHA: t.SHA, Action: audit.Restored, Detail: t.Ref}); err != nil {
				return t, err
			}
		}
		return t, nil
	}
	return gitx.TrashRef{}, fmt.Errorf("cleanup: no trashed branch %q", branch)
}

// PurgeTrash drops trash refs older than retention and returns how many were removed.
func PurgeTrash(ctx context.Context, g Git, j audit.Journal, repo string, retention time.Duration, now time.Time) (int, error) {
	list, err := g.TrashList(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range list {
		if now.Sub(t.DeletedAt) < retention {
			continue
		}
		if err := g.TrashDrop(ctx, t); err != nil {
			return n, err
		}
		n++
		if j != nil {
			_ = j.Append(ctx, audit.Entry{Time: now, Repo: repo, Branch: t.Branch, SHA: t.SHA, Action: audit.TrashPurged, Detail: t.Ref})
		}
	}
	return n, nil
}

func boolTri(b bool) Tri {
	if b {
		return Yes
	}
	return No
}

func hasOutcome(items []Item, o Outcome) bool {
	for _, it := range items {
		if it.Outcome == o {
			return true
		}
	}
	return false
}

// lookupMerged asks the provider only about branches that could newly qualify; failures mean "unknown".
func (in Input) lookupMerged(ctx context.Context, branches []gitx.Branch, merged map[string]bool, haveBase bool) map[string]string {
	if in.ProviderMerged == nil || !haveBase {
		return nil
	}
	var names []string
	for _, b := range branches {
		if b.UpstreamGone && !merged[b.Name] && b.Name != in.DefaultBranch {
			names = append(names, b.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	m, err := in.ProviderMerged(ctx, names)
	if err != nil {
		return nil
	}
	return m
}
