// SPDX-License-Identifier: Apache-2.0

package cleanup

import (
	"testing"
	"time"
)

var (
	now = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old = now.Add(-30 * 24 * time.Hour)
)

func pol() Policy {
	return Policy{Mode: ModeAuto, Protected: DefaultProtected, MinAge: 7 * 24 * time.Hour}
}

func merged() Facts {
	return Facts{Name: "feature/x", CheckedOut: No, TipTime: old, HasUpstream: Yes, MergedIntoDefault: Yes, UpstreamGone: No}
}

func TestDecide_Scenarios(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*Facts, *Policy)
		want Decision
	}{
		{"merged branch", func(*Facts, *Policy) {}, Decision{true, DeleteMerged}},
		{"protected glob", func(f *Facts, _ *Policy) { f.Name = "release/1.2" }, skip(SkipProtected)},
		{"protected exact", func(f *Facts, _ *Policy) { f.Name = "develop" }, skip(SkipProtected)},
		{"default", func(f *Facts, p *Policy) { f.IsDefault = true; p.Protected = nil }, skip(SkipDefault)},
		{"current", func(f *Facts, _ *Policy) { f.IsCurrent = true }, skip(SkipCurrent)},
		{"worktree", func(f *Facts, _ *Policy) { f.CheckedOut = Yes }, skip(SkipCheckedOut)},
		{"worktree unknown", func(f *Facts, _ *Policy) { f.CheckedOut = Unknown }, skip(SkipUnknown)},
		{"too young", func(f *Facts, _ *Policy) { f.TipTime = now.Add(-time.Hour) }, skip(SkipTooYoung)},
		{"tip time unknown", func(f *Facts, _ *Policy) { f.TipTime = time.Time{} }, skip(SkipUnknown)},
		{"never pushed", func(f *Facts, _ *Policy) { f.HasUpstream = No }, skip(SkipNoUpstream)},
		{"never pushed allowed", func(f *Facts, p *Policy) { f.HasUpstream = No; p.AllowNeverPushed = true }, Decision{true, DeleteMerged}},
		{"upstream unknown", func(f *Facts, _ *Policy) { f.HasUpstream = Unknown }, skip(SkipUnknown)},
		{"not merged", func(f *Facts, _ *Policy) { f.MergedIntoDefault = No }, skip(SkipNotMerged)},
		{"merged unknown", func(f *Facts, _ *Policy) { f.MergedIntoDefault = Unknown; f.UpstreamGone = No }, skip(SkipUnknown)},
		{"squash via provider", func(f *Facts, _ *Policy) {
			f.MergedIntoDefault, f.UpstreamGone, f.ProviderMergedTip = No, Yes, Yes
		}, Decision{true, DeleteProviderMerged}},
		{"squash via patch-id", func(f *Facts, _ *Policy) {
			f.MergedIntoDefault, f.UpstreamGone, f.PatchEquivalent = No, Yes, Yes
		}, Decision{true, DeletePatchEquiv}},
		{"gone but no evidence", func(f *Facts, _ *Policy) { f.MergedIntoDefault, f.UpstreamGone = No, Yes }, skip(SkipNotMerged)},
		{"evidence but upstream alive", func(f *Facts, _ *Policy) {
			f.MergedIntoDefault, f.UpstreamGone, f.ProviderMergedTip, f.PatchEquivalent = No, No, Yes, Yes
		}, skip(SkipNotMerged)},
		{"malformed glob protects", func(_ *Facts, p *Policy) { p.Protected = []string{"["} }, skip(SkipProtected)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, p := merged(), pol()
			tc.mod(&f, &p)
			if got := Decide(f, p, now); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Exhaustive property check: over every combination of facts, a delete decision must
// always carry positive merge evidence and never touch a guarded branch.
func TestDecide_Property_DeleteImpliesSafety(t *testing.T) {
	tris := []Tri{Unknown, No, Yes}
	bools := []bool{false, true}
	times := []time.Time{{}, now.Add(-time.Hour), old}
	names := []string{"feature/x", "main", "release/9"}
	var n, deletes int
	for _, name := range names {
		for _, isDef := range bools {
			for _, isCur := range bools {
				for _, co := range tris {
					for _, tt := range times {
						for _, up := range tris {
							for _, mg := range tris {
								for _, gone := range tris {
									for _, pm := range tris {
										for _, pe := range tris {
											for _, allow := range bools {
												f := Facts{name, isDef, isCur, co, tt, up, mg, gone, pm, pe}
												p := Policy{Mode: ModeAuto, Protected: DefaultProtected, MinAge: 7 * 24 * time.Hour, AllowNeverPushed: allow}
												d := Decide(f, p, now)
												n++
												if !d.Delete {
													continue
												}
												deletes++
												switch {
												case isProtected(name, p.Protected), isDef, isCur, co != No, tt.IsZero(), now.Sub(tt) < p.MinAge:
													t.Fatalf("guard violated: %+v => %+v", f, d)
												case up == Unknown, up == No && !allow:
													t.Fatalf("upstream guard violated: %+v => %+v", f, d)
												case mg != Yes && (gone != Yes || (pm != Yes && pe != Yes)):
													t.Fatalf("no merge evidence: %+v => %+v", f, d)
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if deletes == 0 || n < 10000 {
		t.Fatalf("property test degenerate: n=%d deletes=%d", n, deletes)
	}
}

func TestDecide_ZeroFacts_NeverDeletes(t *testing.T) {
	if Decide(Facts{Name: "x"}, Policy{}, now).Delete {
		t.Fatal("zero-value facts must fail closed")
	}
}
