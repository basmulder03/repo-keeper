// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var t0 = time.UnixMilli(1_780_000_000_000)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state", "rk.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, p
}

func at(t time.Time) func(int) time.Time { return func(int) time.Time { return t } }

func TestOpen_CreatesPrivateDBAndIsReopenable(t *testing.T) {
	s, p := open(t)
	_ = s.Close()
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("perm=%v", st.Mode().Perm())
		}
	}
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}

func TestOpen_NewerSchema_Refused(t *testing.T) {
	s, p := open(t)
	if _, err := s.db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+5)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(p); err == nil {
		t.Fatal("must refuse a newer schema")
	}
}

func TestSyncRepos_UpsertDeactivateReactivate(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SyncRepos(ctx, []Spec{{"/a", "origin", time.Hour}, {"/b", "origin", time.Hour}}, at(t0)))
	rs, _ := s.ListRepos(ctx)
	if len(rs) != 2 || rs[0].Path != "/a" || !rs[0].NextSync.Equal(t0) || rs[0].Interval != time.Hour {
		t.Fatalf("rs=%+v", rs)
	}
	must(s.Record(ctx, Run{RepoID: rs[1].ID, Started: t0, Finished: t0, Status: "ok", DefaultBranch: "main", NextSync: t0.Add(time.Hour)}))

	must(s.SyncRepos(ctx, []Spec{{"/a", "up", 2 * time.Hour}}, at(t0)))
	rs, _ = s.ListRepos(ctx)
	if len(rs) != 1 || rs[0].Remote != "up" || rs[0].Interval != 2*time.Hour {
		t.Fatalf("after removal: %+v", rs)
	}

	must(s.SyncRepos(ctx, []Spec{{"/a", "origin", time.Hour}, {"/b", "origin", time.Hour}}, at(t0)))
	rs, _ = s.ListRepos(ctx)
	if len(rs) != 2 || rs[1].DefaultBranch != "main" || rs[1].LastStatus != "ok" {
		t.Fatalf("history of /b must survive: %+v", rs)
	}
}

func TestDue_OrderingLimitAndInactive(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{"/a", "o", time.Hour}, {"/b", "o", time.Hour}, {"/c", "o", time.Hour}},
		func(i int) time.Time { return t0.Add(time.Duration(3-i) * time.Minute) })
	due, err := s.Due(ctx, t0.Add(2*time.Minute), 10)
	if err != nil || len(due) != 2 || due[0].Path != "/c" || due[1].Path != "/b" {
		t.Fatalf("due=%+v err=%v", due, err)
	}
	if due, _ := s.Due(ctx, t0.Add(10*time.Minute), 1); len(due) != 1 {
		t.Fatalf("limit ignored: %+v", due)
	}
}

func TestRecord_UpdatesRepoAndKeepsOldValuesWhenEmpty(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{"/a", "o", time.Hour}}, at(t0))
	r, _ := s.ListRepos(ctx)
	id := r[0].ID

	_ = s.Record(ctx, Run{RepoID: id, Started: t0, Finished: t0.Add(time.Second), Status: "ok", FF: "fast-forwarded", Fetched: true,
		DefaultBranch: "main", Digest: "d1", NextSync: t0.Add(time.Hour)})
	_ = s.Record(ctx, Run{RepoID: id, Started: t0, Finished: t0.Add(time.Minute), Status: "failed", Reason: "fetch", Error: "boom",
		Failures: 2, NeedsAttention: true, NextSync: t0.Add(2 * time.Hour)})

	got, _ := s.Get(ctx, id)
	if got.DefaultBranch != "main" || got.Digest != "d1" {
		t.Fatalf("empty values must not clobber: %+v", got)
	}
	if got.LastStatus != "failed" || got.LastError != "boom" || got.Failures != 2 || !got.NeedsAttention || !got.NextSync.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("got=%+v", got)
	}
	runs, _ := s.RecentRuns(ctx, id, 10)
	if len(runs) != 2 || runs[0].Status != "failed" || !runs[1].Fetched {
		t.Fatalf("runs=%+v", runs)
	}
	if _, err := s.Get(ctx, 999); err == nil {
		t.Fatal("missing repo must error")
	}
}

func TestRecord_TrimsHistory(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{"/a", "o", time.Hour}}, at(t0))
	r, _ := s.ListRepos(ctx)
	for i := 0; i < keepRunsPerRepo+10; i++ {
		_ = s.Record(ctx, Run{RepoID: r[0].ID, Started: t0, Finished: t0, Status: "ok"})
	}
	if runs, _ := s.RecentRuns(ctx, r[0].ID, 1000); len(runs) != keepRunsPerRepo {
		t.Fatalf("runs=%d", len(runs))
	}
}

func TestDeferAndSetHost(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{"/a", "o", time.Hour}}, at(t0))
	r, _ := s.ListRepos(ctx)
	_ = s.Defer(ctx, r[0].ID, t0.Add(time.Hour))
	_ = s.SetHost(ctx, r[0].ID, "github.com")
	got, _ := s.Get(ctx, r[0].ID)
	if !got.NextSync.Equal(t0.Add(time.Hour)) || got.Host != "github.com" || got.LastStatus != "" {
		t.Fatalf("got=%+v", got)
	}
}

func TestEvents_NewestFirstAndBounded(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	for i := 0; i < keepEvents+25; i++ {
		_ = s.AddEvent(ctx, Event{Time: t0, Level: "info", Code: "c", Message: fmt.Sprint(i)})
	}
	ev, err := s.RecentEvents(ctx, keepEvents*2)
	if err != nil || len(ev) > keepEvents+1 || ev[0].Message != fmt.Sprint(keepEvents+24) {
		t.Fatalf("len=%d first=%+v err=%v", len(ev), ev[0], err)
	}
}
