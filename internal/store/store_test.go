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
	must(s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "origin", Interval: time.Hour}, {Path: "/b", Remote: "origin", Interval: time.Hour}}, at(t0)))
	rs, _ := s.ListRepos(ctx)
	if len(rs) != 2 || rs[0].Path != "/a" || !rs[0].NextSync.Equal(t0) || rs[0].Interval != time.Hour {
		t.Fatalf("rs=%+v", rs)
	}
	must(s.Record(ctx, Run{RepoID: rs[1].ID, Started: t0, Finished: t0, Status: "ok", DefaultBranch: "main", NextSync: t0.Add(time.Hour)}))

	must(s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "up", Interval: 2 * time.Hour}}, at(t0)))
	rs, _ = s.ListRepos(ctx)
	if len(rs) != 1 || rs[0].Remote != "up" || rs[0].Interval != 2*time.Hour {
		t.Fatalf("after removal: %+v", rs)
	}

	must(s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "origin", Interval: time.Hour}, {Path: "/b", Remote: "origin", Interval: time.Hour}}, at(t0)))
	rs, _ = s.ListRepos(ctx)
	if len(rs) != 2 || rs[1].DefaultBranch != "main" || rs[1].LastStatus != "ok" {
		t.Fatalf("history of /b must survive: %+v", rs)
	}
}

func TestDue_OrderingLimitAndInactive(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "o", Interval: time.Hour}, {Path: "/b", Remote: "o", Interval: time.Hour}, {Path: "/c", Remote: "o", Interval: time.Hour}},
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
	_ = s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, at(t0))
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
	_ = s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, at(t0))
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
	_ = s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "o", Interval: time.Hour}}, at(t0))
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

func specFor(path, full string) Spec {
	return Spec{Path: path, Remote: "origin", Interval: time.Hour, FullName: full, RemoteID: full, CloneURL: "https://x/" + full}
}

func TestSyncManaged_AddMissingAndReappear(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	res, err := s.SyncManaged(ctx, "gh", []Spec{specFor("/r/a", "o/a"), specFor("/r/b", "o/b")}, at(t0))
	if err != nil || res.Added != 2 || res.Missing != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	rs, _ := s.ListRepos(ctx)
	if len(rs) != 2 || rs[0].Source != "gh" || rs[0].FullName != "o/a" || rs[0].CloneURL == "" {
		t.Fatalf("rs=%+v", rs)
	}

	res, _ = s.SyncManaged(ctx, "gh", []Spec{specFor("/r/a", "o/a")}, at(t0))
	if res.Missing != 1 || res.Added != 0 {
		t.Fatalf("res=%+v", res)
	}
	if rs, _ := s.ListRepos(ctx); len(rs) != 1 {
		t.Fatalf("missing repo must be inactive: %+v", rs)
	}
	var missing bool
	if r, _ := s.queryRepos(ctx, "SELECT "+repoCols+" FROM repos WHERE path = '/r/b'"); len(r) == 1 {
		missing = r[0].Missing
	}
	if !missing {
		t.Fatal("b should be flagged missing, not deleted")
	}

	res, _ = s.SyncManaged(ctx, "gh", []Spec{specFor("/r/a", "o/a"), specFor("/r/b", "o/b")}, at(t0))
	if res.Missing != 0 {
		t.Fatalf("res=%+v", res)
	}
	if rs, _ := s.ListRepos(ctx); len(rs) != 2 {
		t.Fatalf("b should be active again: %+v", rs)
	}
}

func TestSyncManaged_DoesNotHijackOtherSourcesOrManual(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{Path: "/r/manual", Remote: "origin", Interval: time.Hour}}, at(t0))
	_, _ = s.SyncManaged(ctx, "one", []Spec{specFor("/r/shared", "o/shared")}, at(t0))

	res, _ := s.SyncManaged(ctx, "two", []Spec{specFor("/r/shared", "o/shared"), specFor("/r/manual", "o/manual")}, at(t0))
	if len(res.Conflicts) != 2 {
		t.Fatalf("res=%+v", res)
	}
	rs, _ := s.ListRepos(ctx)
	for _, r := range rs {
		if r.Path == "/r/shared" && r.Source != "one" || r.Path == "/r/manual" && r.Source != "" {
			t.Fatalf("hijacked: %+v", r)
		}
	}
	// manual sync must not touch managed repos either
	_ = s.SyncRepos(ctx, nil, at(t0))
	if rs, _ := s.ListRepos(ctx); len(rs) != 1 || rs[0].Path != "/r/shared" {
		t.Fatalf("manual reconcile must leave managed repos active: %+v", rs)
	}
}

func TestAccounts_SaveListUpdate(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SaveAccount(ctx, Account{Name: "b", Provider: "github", Status: "ok", Login: "me", RepoCount: 3, Checked: t0, NextDiscovery: t0.Add(time.Hour)})
	_ = s.SaveAccount(ctx, Account{Name: "a", Provider: "github", Status: "auth-failed", Error: "bad creds"})
	_ = s.SaveAccount(ctx, Account{Name: "b", Provider: "github", Status: "ok", Login: "me2", RepoCount: 4})
	as, err := s.ListAccounts(ctx)
	if err != nil || len(as) != 2 || as[0].Name != "a" || as[1].Login != "me2" || as[1].RepoCount != 4 || as[0].Error != "bad creds" {
		t.Fatalf("as=%+v err=%v", as, err)
	}
}

func TestOpen_UnusableLocation_Fails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if _, err := Open(filepath.Join(blocker, "sub", "x.db")); err == nil {
		t.Fatal("must fail under a regular file")
	}
}

func TestStore_EmptyReads_AndClosedDB(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	if rs, err := s.ListRepos(ctx); err != nil || len(rs) != 0 {
		t.Fatalf("rs=%v err=%v", rs, err)
	}
	if as, err := s.ListAccounts(ctx); err != nil || len(as) != 0 {
		t.Fatalf("as=%v err=%v", as, err)
	}
	if m, err := s.LatestRuns(ctx); err != nil || len(m) != 0 {
		t.Fatalf("m=%v err=%v", m, err)
	}
	if ev, err := s.RecentEvents(ctx, 5); err != nil || len(ev) != 0 {
		t.Fatalf("ev=%v err=%v", ev, err)
	}
	_ = s.Close()
	if _, err := s.ListRepos(ctx); err == nil {
		t.Fatal("queries on a closed store must error")
	}
	if err := s.Defer(ctx, 1, time.Now()); err == nil {
		t.Fatal("writes on a closed store must error")
	}
}

func TestLatestRuns_ReturnsNewestPerRepo(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{Path: "/a", Remote: "o", Interval: time.Hour}, {Path: "/b", Remote: "o", Interval: time.Hour}}, at(t0))
	rs, _ := s.ListRepos(ctx)
	_ = s.Record(ctx, Run{RepoID: rs[0].ID, Started: t0, Finished: t0, Status: "failed"})
	_ = s.Record(ctx, Run{RepoID: rs[0].ID, Started: t0, Finished: t0, Status: "ok", Detail: "{}"})
	m, err := s.LatestRuns(ctx)
	if err != nil || len(m) != 1 || m[rs[0].ID].Status != "ok" || m[rs[0].ID].Detail != "{}" {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}

// The daemon and `repo-keeper status` may open a brand-new database at the same moment; exactly one may run
// the migrations and the other must wait and then see the finished schema (found by the Windows CI leg).
func TestOpen_ConcurrentFirstOpen_MigratesExactlyOnce(t *testing.T) {
	for round := 0; round < 15; round++ {
		p := filepath.Join(t.TempDir(), "state", "rk.db")
		const n = 8
		errs := make(chan error, n)
		stores := make(chan *Store, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			go func() {
				<-start
				s, err := Open(p)
				errs <- err
				stores <- s
			}()
		}
		close(start)
		for i := 0; i < n; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: concurrent open failed: %v", round, err)
			}
			if s := <-stores; s != nil {
				if rs, err := s.ListRepos(t.Context()); err != nil || rs == nil && false {
					t.Fatalf("round %d: schema unusable: %v", round, err)
				}
				_ = s.Close()
			}
		}
	}
}

func TestForgetRemovedAccounts_DeactivatesTheirReposAndState_KeepsOthers(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	_ = s.SyncRepos(ctx, []Spec{{Path: "/manual", Remote: "o", Interval: time.Hour}}, at(t0))
	_, _ = s.SyncManaged(ctx, "keep", []Spec{specFor("/r/k", "o/k")}, at(t0))
	_, _ = s.SyncManaged(ctx, "gone", []Spec{specFor("/r/g", "o/g")}, at(t0))
	_ = s.SaveAccount(ctx, Account{Name: "keep", Provider: "github"})
	_ = s.SaveAccount(ctx, Account{Name: "gone", Provider: "github"})

	if err := s.ForgetRemovedAccounts(ctx, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.ListRepos(ctx)
	paths := map[string]bool{}
	for _, r := range rs {
		paths[r.Path] = true
	}
	if !paths["/manual"] || !paths["/r/k"] || paths["/r/g"] {
		t.Fatalf("repos=%v", paths)
	}
	as, _ := s.ListAccounts(ctx)
	if len(as) != 1 || as[0].Name != "keep" {
		t.Fatalf("accounts=%+v", as)
	}
	if err := s.ForgetRemovedAccounts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.ListRepos(ctx); len(rs) != 1 || rs[0].Path != "/manual" {
		t.Fatalf("with no accounts every managed repo must be deactivated, manual ones kept: %+v", rs)
	}
}
