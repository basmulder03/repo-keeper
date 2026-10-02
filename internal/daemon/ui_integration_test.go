// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/ui"
)

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

type browserSession struct {
	t      *testing.T
	c      *http.Client
	base   string
	csrf   string
	lastBd string
}

// startWithUI runs the daemon with a real loopback UI and signs in exactly like `repo-keeper ui` does.
func startWithUI(t *testing.T, e *gitxtest.Env, cfg string) (*rig, *browserSession) {
	t.Helper()
	dir := t.TempDir()
	r := &rig{e: e, clk: clock.NewFake(time.Now()), cfgPath: filepath.Join(dir, "config.toml"), done: make(chan error, 1)}
	writeCfg(t, r.cfgPath, cfg)
	runtimeDir := filepath.Join(dir, "run")
	r.d = &Daemon{ConfigPath: r.cfgPath, StateDir: filepath.Join(dir, "state"), Runner: e.R, Clock: r.clk, Tick: time.Minute,
		EphemeralUI: true, RuntimeDir: runtimeDir, Version: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- r.d.Run(ctx) }()
	t.Cleanup(func() { r.stop(t) })
	select {
	case <-r.d.Ready():
	case err := <-r.done:
		t.Fatalf("daemon failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}

	var rf ui.RuntimeFile
	r.waitFor(t, "runtime file", func() bool {
		b, err := os.ReadFile(filepath.Join(runtimeDir, "ui.json"))
		return err == nil && json.Unmarshal(b, &rf) == nil && rf.Control != ""
	})
	jar, _ := cookiejar.New(nil)
	bs := &browserSession{t: t, c: &http.Client{Jar: jar, Timeout: 10 * time.Second}, base: "http://" + rf.Addr}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, bs.base+"/api/login-url", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+rf.Control)
	resp, err := bs.c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login-url: %v %v", err, resp)
	}
	var out struct{ URL string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if !strings.Contains(out.URL, "/login?code=") {
		t.Fatalf("url=%q", out.URL)
	}
	lu, _ := url.Parse(out.URL)
	if code, _ := bs.get(lu.RequestURI()); code != 200 { // follows the 303 to the dashboard
		t.Fatalf("login did not land on the dashboard: %d", code)
	}
	return r, bs
}

func (b *browserSession) fetch(req *http.Request) (int, string) {
	b.t.Helper()
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	body := string(data)
	if m := csrfRe.FindStringSubmatch(body); m != nil {
		b.csrf = m[1]
	}
	b.lastBd = body
	return resp.StatusCode, body
}

func (b *browserSession) get(path string) (int, string) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, b.base+path, http.NoBody)
	return b.fetch(req)
}

func (b *browserSession) post(path string, form url.Values) (int, string) {
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", b.csrf)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.fetch(req)
}

func TestUI_EndToEnd_SyncNowConfigCleanupRestoreAuditDebug(t *testing.T) {
	e := gitxtest.New(t)
	// a pushed, then merged-and-deleted branch exists locally
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f.txt", "f", "f")
	e.Git(e.Work, "push", "-q", "-u", "origin", "feat")
	e.Git(e.Work, "checkout", "-q", "main")
	e.Git(e.Work, "merge", "-q", "--no-ff", "-m", "m", "feat")
	e.Git(e.Work, "push", "-q", "origin", "main")
	e.Git(e.Origin, "branch", "-D", "feat")

	cfg := "[general]\ninterval = \"30m\"\n[cleanup]\nmode = \"dry-run\"\nmin_age = \"0s\"\n[[repo]]\npath = \"" + e.Work + "\"\n"
	r, b := startWithUI(t, e, cfg)
	r.waitFor(t, "first sync", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].LastStatus == "ok" })

	// dashboard lists the repo; the cleanup page shows the dry-run candidate
	if code, body := b.get("/"); code != 200 || !strings.Contains(body, e.Work) || !strings.Contains(body, "up to date") {
		t.Fatalf("dashboard: %d\n%.600s", code, body)
	}
	if code, body := b.get("/cleanup"); code != 200 || !strings.Contains(body, "would-delete") || !strings.Contains(body, "feat") {
		t.Fatalf("cleanup page: %d\n%.800s", code, body)
	}

	// "Sync now" picks up a teammate's push immediately, without waiting for the interval
	other := e.Clone("t1")
	want := e.Commit(other, "n.txt", "n", "n")
	e.Git(other, "push", "-q", "origin", "main")
	repo := r.repos(t)[0]
	b.get("/")
	if code, _ := b.post("/repos/"+itoa(repo.ID)+"/sync", url.Values{"next": {"/"}}); code != 200 {
		t.Fatalf("sync now: %d", code)
	}
	r.waitFor(t, "manual sync applied", func() bool { return e.Git(e.Work, "rev-parse", "main") == want })

	// approve cleanup: the branch is deleted, trashed and journaled
	b.get("/repos/" + itoa(repo.ID))
	var code int
	var body string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if code, body = b.post("/repos/"+itoa(repo.ID)+"/cleanup", nil); code != http.StatusConflict { // 409 = a sync still holds the repo
			break
		}
	}
	if code != 200 || !strings.Contains(body, "Cleanup finished") {
		t.Fatalf("cleanup now: %d\n%.600s", code, body)
	}
	if _, err := e.R.Run(context.Background(), e.Work, "rev-parse", "--verify", "-q", "refs/heads/feat"); err == nil {
		t.Fatal("feat should be deleted")
	}
	_, body = b.get("/repos/" + itoa(repo.ID))
	if !strings.Contains(body, "Restore") || !strings.Contains(body, "feat") {
		t.Fatalf("trash entry missing:\n%.1500s", body)
	}
	if code, _ := b.post("/repos/"+itoa(repo.ID)+"/restore", url.Values{"branch": {"feat"}}); code != 200 {
		t.Fatalf("restore: %d", code)
	}
	if _, err := e.R.Run(context.Background(), e.Work, "rev-parse", "--verify", "-q", "refs/heads/feat"); err != nil {
		t.Fatal("feat should be restored")
	}
	if _, body := b.get("/audit"); !strings.Contains(body, "deleted") || !strings.Contains(body, "restored") {
		t.Fatalf("audit page lacks entries:\n%.1200s", body)
	}

	// debug page shows redacted git commands and the bundle is valid JSON
	if code, body := b.get("/debug"); code != 200 || !strings.Contains(body, "git ") || !strings.Contains(body, "ls-remote") {
		t.Fatalf("debug: %d\n%.600s", code, body)
	}
	_, raw := b.get("/debug/bundle.json")
	var bundle map[string]any
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil || bundle["commands"] == nil || bundle["repos"] == nil {
		t.Fatalf("bundle invalid: %v\n%.300s", err, raw)
	}

	// config editing: invalid is rejected with reasons, valid is applied by the running daemon
	_, page := b.get("/config")
	if !strings.Contains(page, "min_age") {
		t.Fatalf("config page: %.400s", page)
	}
	ver := regexp.MustCompile(`name="version" value="([^"]+)"`).FindStringSubmatch(page)[1]
	code, body = b.post("/config", url.Values{"version": {ver}, "text": {"[general]\ninterval = \"1m\"\n"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "below the 5m0s minimum") {
		t.Fatalf("invalid config: %d\n%.500s", code, body)
	}
	e2 := e.Clone("second")
	newCfg := cfg + "[[repo]]\npath = \"" + e2 + "\"\n"
	if code, body := b.post("/config", url.Values{"version": {ver}, "text": {newCfg}}); code != 200 || !strings.Contains(body, "Configuration saved") {
		t.Fatalf("save: %d\n%.500s", code, body)
	}
	r.clk.BlockUntil(3, time.Second)
	r.waitFor(t, "second repo tracked after UI save", func() bool { return len(r.repos(t)) == 2 })
	hist, _ := os.ReadDir(filepath.Join(r.d.StateDir, "config-history"))
	if len(hist) != 1 {
		t.Fatalf("expected one archived config, got %d", len(hist))
	}
	// the same (now stale) version must be refused instead of silently overwriting
	if code, body := b.post("/config", url.Values{"version": {ver}, "text": {cfg}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "changed on disk") {
		t.Fatalf("stale save: %d\n%.500s", code, body)
	}
	if fi, _ := os.Stat(r.cfgPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode().Perm())
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestUI_NoUIFlag_AndDisabledInConfig(t *testing.T) {
	e := gitxtest.New(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	writeCfg(t, cfg, "[ui]\nenabled = false\n")
	d := &Daemon{ConfigPath: cfg, StateDir: filepath.Join(dir, "state"), Runner: e.R, RuntimeDir: filepath.Join(dir, "run")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(dir, "run", "ui.json")); err == nil {
		t.Fatal("UI started although disabled")
	}
}

func TestUI_MachineAPI_StatusPauseSyncAll(t *testing.T) {
	e := gitxtest.New(t)
	cfg := "[general]\ninterval = \"30m\"\n[cleanup]\nmode = \"off\"\n[[repo]]\npath = \"" + e.Work + "\"\n"
	r, b := startWithUI(t, e, cfg)
	r.waitFor(t, "first sync", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].LastStatus == "ok" })

	var rf ui.RuntimeFile
	data, _ := os.ReadFile(filepath.Join(r.d.RuntimeDir, "ui.json"))
	_ = json.Unmarshal(data, &rf)
	call := func(method, path string) (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), method, "http://"+rf.Addr+path, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+rf.Control)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		bd, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(bd)
	}
	var st ui.Status
	code, body := call(http.MethodGet, "/api/status")
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || st.Repos != 1 || st.UpToDate != 1 || st.Paused || st.Version != "test" {
		t.Fatalf("status: %d %s", code, body)
	}

	// paused: a teammate's push is NOT picked up on schedule, but "sync all" still works
	if code, _ := call(http.MethodPost, "/api/pause"); code != http.StatusNoContent {
		t.Fatalf("pause: %d", code)
	}
	if _, body := call(http.MethodGet, "/api/status"); !strings.Contains(body, `"paused":true`) {
		t.Fatalf("status after pause: %s", body)
	}
	other := e.Clone("t1")
	want := e.Commit(other, "n.txt", "n", "n")
	e.Git(other, "push", "-q", "origin", "main")
	r.clk.BlockUntil(3, time.Second)
	r.clk.Advance(2 * time.Hour)
	time.Sleep(150 * time.Millisecond)
	if e.Git(e.Work, "rev-parse", "main") == want {
		t.Fatal("scheduled sync ran while paused")
	}
	if code, _ := call(http.MethodPost, "/api/sync-all"); code != http.StatusNoContent {
		t.Fatalf("sync-all: %d", code)
	}
	r.waitFor(t, "sync-all applied while paused", func() bool { return e.Git(e.Work, "rev-parse", "main") == want })
	if code, _ := call(http.MethodPost, "/api/resume"); code != http.StatusNoContent {
		t.Fatalf("resume: %d", code)
	}
	_ = b
}
