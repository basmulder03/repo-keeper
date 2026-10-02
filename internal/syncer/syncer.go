// SPDX-License-Identifier: Apache-2.0

// Package syncer runs one repository through detect -> fetch -> fast-forward -> cleanup.
package syncer

import (
	"context"
	"errors"
	"fmt"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
)

// Git is what the syncer needs; *gitx.Repo implements it.
type Git interface {
	cleanup.Git
	Lock(ctx context.Context) (func(), error)
	CheckSafeConfig(ctx context.Context) error
	LsRemote(ctx context.Context, remote string) (gitx.RemoteState, error)
	OriginHead(ctx context.Context, remote string) (string, error)
	SetOriginHead(ctx context.Context, remote, branch string) error
	Fetch(ctx context.Context, remote string, allBranches bool, branch string) error
	IsAncestor(ctx context.Context, a, b string) (bool, error)
	MergeFFOnly(ctx context.Context, target string) error
}

// Input configures one sync of one repository.
type Input struct {
	Git         Git
	Path        string // journal label
	Remote      string // default "origin"
	AllBranches bool
	// PrevDigest from the last run; an unchanged remote skips the network fetch.
	PrevDigest string
	Cleanup    cleanup.Policy
	Journal    audit.Journal
	Clock      clock.Clock
	// ProviderMerged feeds squash-merge detection (branch -> merged PR head SHA); optional.
	ProviderMerged map[string]string
}

// Status is the overall outcome.
type Status string

// Overall statuses.
const (
	OK      Status = "ok"
	Skipped Status = "skipped"
	Failed  Status = "failed"
)

// FF is the outcome of updating the default branch.
type FF string

// Fast-forward outcomes; everything except UpToDate/FastForwarded left the branch untouched.
const (
	UpToDate          FF = "up-to-date"
	FastForwarded     FF = "fast-forwarded"
	Ahead             FF = "local-ahead"
	Diverged          FF = "diverged"
	SkippedDirty      FF = "skipped-dirty"
	SkippedWorktree   FF = "skipped-other-worktree"
	SkippedNoLocal    FF = "skipped-no-local-branch"
	SkippedNoRemote   FF = "skipped-no-remote-branch"
	SkippedInProgress FF = "skipped-operation-in-progress"
	NotAttempted      FF = ""
)

// Result is the full record of a sync, consumed by the store/UI later.
type Result struct {
	Status         Status
	Reason         string
	DefaultBranch  string
	PrevDefault    string // origin/HEAD before this run, if it differed
	DefaultChanged bool
	Fetched        bool
	Digest         string
	FF             FF
	Cleanup        *cleanup.Report
	Err            error
}

// Sync never modifies anything it cannot prove safe; every skip is reported, not hidden.
func Sync(ctx context.Context, in Input) Result {
	remote := in.Remote
	if remote == "" {
		remote = "origin"
	}
	g := in.Git

	unlock, err := g.Lock(ctx)
	if err != nil {
		if errors.Is(err, gitx.ErrLocked) {
			return Result{Status: Skipped, Reason: "locked", Err: err}
		}
		return fail("lock", err)
	}
	defer unlock()

	if err := g.CheckSafeConfig(ctx); err != nil {
		return fail("unsafe-config", err)
	}

	res := Result{Status: OK}
	remoteState, err := g.LsRemote(ctx, remote)
	if err != nil {
		return fail("ls-remote", err)
	}
	res.Digest = remoteState.Digest

	prevHead, err := g.OriginHead(ctx, remote)
	if err != nil {
		return fail("origin-head", err)
	}
	def := remoteState.DefaultBranch
	if def == "" {
		def = prevHead // remote advertises no HEAD symref: keep what we had
	}

	if in.PrevDigest == "" || in.PrevDigest != remoteState.Digest || def != prevHead {
		if err := g.Fetch(ctx, remote, in.AllBranches, def); err != nil {
			res.Digest = "" // do not remember a digest for a failed fetch
			r := fail("fetch", err)
			r.DefaultBranch = def
			return r
		}
		res.Fetched = true
	}

	if def == "" {
		def = probeDefault(ctx, g, remote)
	}
	if def == "" {
		res.Status, res.Reason = Skipped, "default-branch-unknown"
		return res
	}
	res.DefaultBranch = def
	if def != prevHead {
		if err := g.SetOriginHead(ctx, remote, def); err != nil {
			return fail("set-origin-head", err)
		}
		res.PrevDefault, res.DefaultChanged = prevHead, prevHead != ""
	}

	if op, err := g.InProgress(ctx); err != nil {
		return fail("in-progress", err)
	} else if op != "" {
		res.FF = SkippedInProgress
		res.Reason = "operation-in-progress:" + op
		return res
	}

	res.FF, err = fastForward(ctx, g, remote, def)
	if err != nil {
		res.Status, res.Reason, res.Err = Failed, "fast-forward", err
		return res
	}

	if in.Cleanup.Mode != cleanup.ModeOff && in.Cleanup.Mode != "" {
		rep, err := cleanup.Run(ctx, cleanup.Input{
			Git: g, Repo: in.Path, Remote: remote, DefaultBranch: def, Policy: in.Cleanup,
			Journal: in.Journal, Clock: in.Clock, ProviderMerged: in.ProviderMerged,
		})
		res.Cleanup = &rep
		if err != nil {
			res.Status, res.Reason, res.Err = Failed, "cleanup", err
		}
	}
	return res
}

func fail(reason string, err error) Result {
	return Result{Status: Failed, Reason: reason, Err: fmt.Errorf("%s: %w", reason, err)}
}

// probeDefault is the last resort when neither the remote nor origin/HEAD names a default.
func probeDefault(ctx context.Context, g Git, remote string) string {
	for _, b := range []string{"main", "master"} {
		if _, ok, _ := g.RevParse(ctx, "refs/remotes/"+remote+"/"+b); ok {
			return b
		}
	}
	return ""
}

// fastForward moves the local default branch to the remote tip, never anything but forward.
func fastForward(ctx context.Context, g Git, remote, def string) (FF, error) {
	local, ok, err := g.RevParse(ctx, "refs/heads/"+def)
	if err != nil {
		return NotAttempted, err
	}
	if !ok {
		return SkippedNoLocal, nil
	}
	remoteRef := "refs/remotes/" + remote + "/" + def
	target, ok, err := g.RevParse(ctx, remoteRef)
	if err != nil {
		return NotAttempted, err
	}
	if !ok {
		return SkippedNoRemote, nil
	}
	if local == target {
		return UpToDate, nil
	}
	canFF, err := g.IsAncestor(ctx, local, target)
	if err != nil {
		return NotAttempted, err
	}
	if !canFF {
		if behind, err := g.IsAncestor(ctx, target, local); err == nil && behind {
			return Ahead, nil
		}
		return Diverged, nil
	}

	current, _, err := g.CurrentBranch(ctx)
	if err != nil {
		return NotAttempted, err
	}
	if current == def {
		dirty, err := g.Dirty(ctx, false)
		if err != nil {
			return NotAttempted, err
		}
		if dirty {
			return SkippedDirty, nil
		}
		if err := g.MergeFFOnly(ctx, remoteRef); err != nil {
			return NotAttempted, err
		}
		return FastForwarded, nil
	}
	wts, err := g.Worktrees(ctx)
	if err != nil {
		return NotAttempted, err
	}
	if wts[def] != "" {
		return SkippedWorktree, nil
	}
	if err := g.UpdateRef(ctx, "refs/heads/"+def, target, local); err != nil {
		return NotAttempted, err
	}
	return FastForwarded, nil
}
