// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/config"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/store"
	"github.com/basmulder03/repo-keeper/internal/update"
)

type updateRig struct {
	d      *Daemon
	clk    *clock.Fake
	hits   *atomic.Int32
	srv    *httptest.Server
	reply  *atomic.Value // string body
	status *atomic.Int32
}

func newUpdateRig(t *testing.T, version string, cfg config.Config) *updateRig {
	t.Helper()
	r := &updateRig{hits: &atomic.Int32{}, reply: &atomic.Value{}, status: &atomic.Int32{}}
	r.reply.Store(`[]`)
	r.status.Store(200)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		if req.URL.Path != "/repos/basmulder03/repo-keeper/releases" {
			http.NotFound(w, req)
			return
		}
		w.WriteHeader(int(r.status.Load()))
		_, _ = w.Write([]byte(r.reply.Load().(string)))
	}))
	t.Cleanup(r.srv.Close)
	r.clk = clock.NewFake(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	hc, err := httpx.New(httpx.Config{UserAgent: "repo-keeper/test", Limiter: ratelimit.New(ratelimit.Config{Rate: 1000, Burst: 1000}, clock.Real{}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	buf := &syncBuf{}
	r.d = &Daemon{StateDir: dir, Clock: r.clk, Log: newTestLog(buf).logger, Store: st, HTTP: hc, Version: version, UpdateAPI: r.srv.URL, Redactor: newTestLog(buf).red}
	r.d.live.Store(r.d.newLive(cfg))
	return r
}

const betaReleases = `[
 {"tag_name":"v0.1.0-beta.6","prerelease":true,"html_url":"https://github.com/basmulder03/repo-keeper/releases/tag/v0.1.0-beta.6","published_at":"2026-10-04T00:00:00Z","body":"### Added\n- things"},
 {"tag_name":"v0.1.0-beta.5","prerelease":true,"html_url":"https://github.com/basmulder03/repo-keeper/releases/tag/v0.1.0-beta.5","published_at":"2026-10-03T00:00:00Z"}]`

func (r *updateRig) events(t *testing.T) []store.Event {
	t.Helper()
	evs, err := r.d.Store.RecentEvents(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range evs {
		if e.Code == "update-available" {
			out = append(out, e)
		}
	}
	return out
}

func TestJittered_StaysWithinTheFraction(t *testing.T) {
	for _, r := range []float64{0, 0.25, 0.5, 0.75, 1} {
		got := jittered(24*time.Hour, 0.1, r)
		if got < time.Duration(21.6*float64(time.Hour)) || got > time.Duration(26.4*float64(time.Hour)) {
			t.Errorf("r=%v: %v", r, got)
		}
	}
	if jittered(time.Hour, 0.1, 0.5) != time.Hour {
		t.Fatal("the middle of the random range must not move the base")
	}
}

func TestCheckForUpdate_NewerRelease_RecordsStateAndTellsOnce(t *testing.T) {
	r := newUpdateRig(t, "0.1.0-beta.5", config.Default())
	r.reply.Store(betaReleases)
	cur := mustParse(t, "0.1.0-beta.5")
	r.d.checkForUpdate(t.Context(), cur, update.ReadState(r.d.StateDir))
	st := update.ReadState(r.d.StateDir)
	if st.Latest != "0.1.0-beta.6" || st.Urgent || !st.Available("0.1.0-beta.5") || st.Notified != "0.1.0-beta.6" {
		t.Fatalf("state=%+v", st)
	}
	if st.Next.Before(r.clk.Now().Add(21*time.Hour)) || st.Next.After(r.clk.Now().Add(27*time.Hour)) {
		t.Fatalf("next check must be about a day away, jittered: %v", st.Next.Sub(r.clk.Now()))
	}
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "repo-keeper update") {
		t.Fatalf("events=%+v", evs)
	}
	r.d.checkForUpdate(t.Context(), cur, update.ReadState(r.d.StateDir))
	if len(r.events(t)) != 1 {
		t.Fatal("the same version must be announced only once")
	}
	if r.hits.Load() != 2 {
		t.Fatalf("hits=%d", r.hits.Load())
	}
}

func TestCheckForUpdate_SecurityRelease_IsWarnedAboutAndMarkedUrgent(t *testing.T) {
	r := newUpdateRig(t, "0.1.0-beta.5", config.Default())
	r.reply.Store(`[{"tag_name":"v0.1.0-beta.6","prerelease":true,"html_url":"https://github.com/basmulder03/repo-keeper/releases/tag/v0.1.0-beta.6","body":"### Security\n- fixed CVE-2026-1234"}]`)
	r.d.checkForUpdate(t.Context(), mustParse(t, "0.1.0-beta.5"), update.State{})
	evs := r.events(t)
	if st := update.ReadState(r.d.StateDir); !st.Urgent || len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "security fix") {
		t.Fatalf("state=%+v events=%+v", st, evs)
	}
	if v, _, urgent := r.d.updateNotice(); v != "0.1.0-beta.6" || !urgent {
		t.Fatalf("the UI notice must carry the urgency: %v %v", v, urgent)
	}
}

func TestCheckForUpdate_UpToDate_SaysNothing_OlderStateIsCleared(t *testing.T) {
	r := newUpdateRig(t, "0.1.0-beta.6", config.Default())
	r.reply.Store(betaReleases)
	old := update.State{Latest: "0.1.0-beta.6", Notified: "0.1.0-beta.6"}
	r.d.checkForUpdate(t.Context(), mustParse(t, "0.1.0-beta.6"), old)
	st := update.ReadState(r.d.StateDir)
	if st.Available("0.1.0-beta.6") || len(r.events(t)) != 0 {
		t.Fatalf("up to date must be silent: %+v", st)
	}
	r.reply.Store(`[]`)
	r.d.checkForUpdate(t.Context(), mustParse(t, "0.1.0-beta.6"), st)
	if got := update.ReadState(r.d.StateDir); got.Latest != "" {
		t.Fatalf("no releases at all must clear the remembered one: %+v", got)
	}
}

func TestCheckForUpdate_StableInstallIsNotToldAboutBetas(t *testing.T) {
	r := newUpdateRig(t, "0.1.0", config.Default())
	r.reply.Store(betaReleases)
	r.d.checkForUpdate(t.Context(), mustParse(t, "0.1.0"), update.State{})
	if st := update.ReadState(r.d.StateDir); st.Latest != "" || len(r.events(t)) != 0 {
		t.Fatalf("state=%+v", st)
	}
}

func TestCheckForUpdate_Failure_IsSilent_KeepsWhatWasKnown_RetriesLater(t *testing.T) {
	r := newUpdateRig(t, "0.1.0-beta.5", config.Default())
	r.status.Store(500)
	known := update.State{Checked: r.clk.Now().Add(-48 * time.Hour), Latest: "0.1.0-beta.6", URL: "https://github.com/x", Notified: "0.1.0-beta.6"}
	r.d.checkForUpdate(t.Context(), mustParse(t, "0.1.0-beta.5"), known)
	st := update.ReadState(r.d.StateDir)
	if st.Latest != "0.1.0-beta.6" || !st.Checked.Equal(known.Checked) || len(r.events(t)) != 0 {
		t.Fatalf("a failed check must change nothing the user sees: %+v", st)
	}
	if d := st.Next.Sub(r.clk.Now()); d < 5*time.Hour || d > 7*time.Hour {
		t.Fatalf("retry must be hours away, not a hammering loop: %v", d)
	}
}

func TestUpdateTick_Schedule(t *testing.T) {
	off := config.Default()
	f := false
	off.Update.Check = &f
	for name, tc := range map[string]struct {
		version string
		cfg     config.Config
		prep    func(r *updateRig)
		hits    int32
	}{
		"disabled in config": {"0.1.0-beta.5", off, nil, 0},
		"development build":  {"dev", config.Default(), nil, 0},
		"git describe build": {"0166000-dirty", config.Default(), nil, 0},
		"first start waits":  {"0.1.0-beta.5", config.Default(), nil, 0},
		"not due yet": {"0.1.0-beta.5", config.Default(), func(r *updateRig) {
			_ = update.WriteState(r.d.StateDir, update.State{Next: r.clk.Now().Add(5 * time.Hour)})
		}, 0},
		"due": {"0.1.0-beta.5", config.Default(), func(r *updateRig) {
			_ = update.WriteState(r.d.StateDir, update.State{Next: r.clk.Now().Add(-time.Minute)})
		}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newUpdateRig(t, tc.version, tc.cfg)
			r.reply.Store(betaReleases)
			if tc.prep != nil {
				tc.prep(r)
			}
			wait := r.d.updateTick(t.Context())
			if r.hits.Load() != tc.hits || wait <= 0 {
				t.Fatalf("hits=%d (want %d) wait=%v", r.hits.Load(), tc.hits, wait)
			}
		})
	}
	r := newUpdateRig(t, "0.1.0-beta.5", config.Default())
	r.d.updateTick(t.Context())
	st := update.ReadState(r.d.StateDir)
	if d := st.Next.Sub(r.clk.Now()); d < time.Minute || d > 5*time.Minute {
		t.Fatalf("first check about %v after a fresh start, want a few minutes", d)
	}
}

func TestUpdateNotice_OnlyForTheProjectsOwnLinks_AndOnlyWhenEnabled(t *testing.T) {
	r := newUpdateRig(t, "0.1.0-beta.5", config.Default())
	_ = update.WriteState(r.d.StateDir, update.State{Latest: "0.1.0-beta.6", URL: "https://evil.example/phish"})
	if v, u, _ := r.d.updateNotice(); v != "0.1.0-beta.6" || u != "" {
		t.Fatalf("a link that is not github.com must be dropped: %q %q", v, u)
	}
	_ = update.WriteState(r.d.StateDir, update.State{Latest: "0.1.0-beta.6", URL: "https://github.com/basmulder03/repo-keeper/releases/tag/v0.1.0-beta.6"})
	if _, u, _ := r.d.updateNotice(); !strings.HasPrefix(u, "https://github.com/") {
		t.Fatalf("u=%q", u)
	}
	off := config.Default()
	f := false
	off.Update.Check = &f
	r.d.live.Store(r.d.newLive(off))
	if v, _, _ := r.d.updateNotice(); v != "" {
		t.Fatal("opted out: the interface must show nothing even from an old state file")
	}
}

func mustParse(t *testing.T, s string) update.Version {
	t.Helper()
	v, err := update.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
