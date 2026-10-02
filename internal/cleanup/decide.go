// SPDX-License-Identifier: Apache-2.0

// Package cleanup decides which local branches are safe to delete and removes them recoverably.
package cleanup

import (
	"path"
	"time"
)

// Tri is a fact that may be unknown; unknown always fails closed.
type Tri uint8

// Tri values; the zero value is Unknown on purpose.
const (
	Unknown Tri = iota
	No
	Yes
)

// Mode selects what cleanup does with safe candidates.
type Mode string

// Cleanup modes.
const (
	ModeOff    Mode = "off"
	ModeDryRun Mode = "dry-run"
	ModeAuto   Mode = "auto"
)

// Reason explains a decision; stable strings, shown in the UI and audit journal.
type Reason string

// Skip reasons (never delete) and delete reasons (evidence of merge).
const (
	SkipProtected  Reason = "protected"
	SkipDefault    Reason = "default-branch"
	SkipCurrent    Reason = "current-branch"
	SkipCheckedOut Reason = "checked-out-in-worktree"
	SkipTooYoung   Reason = "too-young"
	SkipNoUpstream Reason = "never-pushed"
	SkipNotMerged  Reason = "not-merged"
	SkipUnknown    Reason = "insufficient-information"

	DeleteMerged         Reason = "merged-into-default"
	DeleteProviderMerged Reason = "pr-merged-tip-matches"
	DeletePatchEquiv     Reason = "squash-merged-patch-equivalent"
)

// Facts is everything the predicate may use; all gathering happens outside so this stays pure.
type Facts struct {
	Name string
	// Identity facts are plain bools: a wrong default cannot make a delete more likely.
	IsDefault bool
	IsCurrent bool
	// CheckedOut: is the branch checked out in any worktree.
	CheckedOut Tri
	// TipTime is the tip commit's committer time; zero = unknown.
	TipTime time.Time
	// HasUpstream: a tracking branch was ever configured (guards fresh, empty, never-pushed branches).
	HasUpstream Tri
	// MergedIntoDefault: tip reachable from the default branch (local or origin).
	MergedIntoDefault Tri
	// UpstreamGone: configured upstream no longer exists on the remote.
	UpstreamGone Tri
	// ProviderMergedTip: provider reports a merged PR whose head SHA equals the local tip.
	ProviderMergedTip Tri
	// PatchEquivalent: every commit has a patch-equal commit on the default branch (git cherry).
	PatchEquivalent Tri
}

// Policy is the user-configurable part of the decision.
type Policy struct {
	Mode      Mode
	Protected []string // path.Match globs
	MinAge    time.Duration
	// AllowNeverPushed permits deleting merged branches that never had an upstream.
	AllowNeverPushed bool
}

// DefaultProtected are never deleted unless the user replaces the list.
var DefaultProtected = []string{"main", "master", "trunk", "develop", "dev", "staging", "production", "release/*", "hotfix/*"}

// Decision is the outcome for one branch.
type Decision struct {
	Delete bool
	Reason Reason
}

func skip(r Reason) Decision { return Decision{Reason: r} }

// Decide is the safety predicate: delete only with positive evidence, otherwise skip.
func Decide(f Facts, p Policy, now time.Time) Decision {
	switch {
	case isProtected(f.Name, p.Protected):
		return skip(SkipProtected)
	case f.IsDefault:
		return skip(SkipDefault)
	case f.IsCurrent:
		return skip(SkipCurrent)
	case f.CheckedOut != No:
		if f.CheckedOut == Yes {
			return skip(SkipCheckedOut)
		}
		return skip(SkipUnknown)
	case f.TipTime.IsZero():
		return skip(SkipUnknown)
	case now.Sub(f.TipTime) < p.MinAge:
		return skip(SkipTooYoung)
	}

	// Never-pushed branches are ambiguous (a fresh empty branch looks "merged"), so they need opt-in.
	if f.HasUpstream == Unknown {
		return skip(SkipUnknown)
	}
	if f.HasUpstream == No && !p.AllowNeverPushed {
		return skip(SkipNoUpstream)
	}

	if f.MergedIntoDefault == Yes {
		return Decision{Delete: true, Reason: DeleteMerged}
	}
	if f.UpstreamGone == Yes {
		switch {
		case f.ProviderMergedTip == Yes:
			return Decision{Delete: true, Reason: DeleteProviderMerged}
		case f.PatchEquivalent == Yes:
			return Decision{Delete: true, Reason: DeletePatchEquiv}
		}
	}

	if f.MergedIntoDefault == Unknown || (f.UpstreamGone == Unknown && f.MergedIntoDefault != Yes) {
		return skip(SkipUnknown)
	}
	return skip(SkipNotMerged)
}

func isProtected(name string, globs []string) bool {
	for _, g := range globs {
		if ok, err := path.Match(g, name); err != nil || ok { // a malformed glob protects everything
			return true
		}
	}
	return false
}
