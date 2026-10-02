// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/store"
)

const (
	addr = "127.0.0.1:7878"
	xss  = `<script>alert(1)</script>`
)

type fake struct {
	mu         sync.Mutex
	calls      []string
	saveErr    error
	restoreEr  error
	cleanupErr error
	readOnly   string
	report     cleanup.Report
}

func (f *fake) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fake) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

var now = time.Now()

func (f *fake) Info() Info {
	return Info{Version: "1.2.3", GitVersion: "2.50.0", GoVersion: "go1.26", ConfigPath: "/cfg", StateDir: "/state", UIAddr: addr, Started: now}
}
func (f *fake) Repos(context.Context) ([]store.Repo, error) {
	return []store.Repo{
		{ID: 1, Path: "/code/" + xss, FullName: "acme/<b>api</b>", Source: "gh", DefaultBranch: "main", LastStatus: "ok", LastFF: "fast-forwarded", LastSync: now, NextSync: now.Add(time.Hour), Active: true},
		{ID: 2, Path: "/code/b", NeedsAttention: true, LastStatus: "failed", LastReason: "fetch", LastError: "boom " + xss, Active: true},
		{ID: 3, Path: "/code/c", Active: true},
	}, nil
}
func (f *fake) Repo(_ context.Context, id int64) (store.Repo, error) {
	rs, _ := f.Repos(context.Background())
	for _, r := range rs {
		if r.ID == id {
			return r, nil
		}
	}
	return store.Repo{}, errors.New("nope")
}
func (f *fake) Runs(context.Context, int64, int) ([]store.Run, error) {
	rep, _ := json.Marshal(f.report)
	return []store.Run{{Status: "ok", Finished: now, Started: now.Add(-time.Second), Detail: string(rep), Error: xss}}, nil
}
func (f *fake) Accounts(context.Context) ([]store.Account, error) {
	return []store.Account{{Name: "gh", Provider: "github", Status: "auth-failed", Error: "bad " + xss, Login: "octo", Warnings: "broad scope", RepoCount: 3}}, nil
}
func (f *fake) Events(context.Context, int) ([]store.Event, error) {
	return []store.Event{{Time: now, Level: "warn", Code: "sync-failed", Message: "x " + xss}}, nil
}
func (f *fake) Hosts() []ratelimit.HostState {
	return []ratelimit.HostState{{Host: "api.github.com", Quota: ratelimit.Quota{Limit: 5000, Remaining: 4000, Reset: now}, Cooldown: now.Add(time.Minute), CooldownBy: "rate-limited"}}
}
func (f *fake) Commands() []gitx.CommandRecord {
	return []gitx.CommandRecord{{Time: now, Dir: "/code/a", Args: []string{"fetch", "--prune"}}, {Time: now, Dir: "/code/b", Args: []string{"merge", xss}, Exit: 1, Err: "fail"}}
}
func (f *fake) Cleanups(context.Context) ([]CleanupView, error) {
	r, _ := f.Repo(context.Background(), 1)
	return []CleanupView{{Repo: r, Mode: "dry-run", Report: f.report}}, nil
}
func (f *fake) Audit(context.Context, int) ([]audit.Entry, error) {
	return []audit.Entry{
		{Time: now, Repo: "/code/" + xss, Branch: "feat/" + xss, SHA: "abcdef0123456789", Action: audit.Deleted, Reason: "merged-into-default"},
		{Time: now, Repo: "/code/b", Branch: "old", Action: audit.Restored},
	}, nil
}
func (f *fake) Trash(context.Context, int64) ([]gitx.TrashRef, error) {
	return []gitx.TrashRef{{Branch: "gone/" + xss, SHA: "0123456789abcdef", DeletedAt: now}}, nil
}
func (f *fake) Status(context.Context) (Status, error) {
	return Status{Version: "1.2.3", Repos: 3, UpToDate: 1, NeedAttention: 1, Pending: 1, Accounts: 1, AccountsAttention: 1}, nil
}
func (f *fake) SyncAll(context.Context) error { f.rec("sync-all"); return nil }
func (f *fake) Shutdown(restart bool) {
	if restart {
		f.rec("restart")
	} else {
		f.rec("shutdown")
	}
}
func (f *fake) SetPaused(p bool) {
	if p {
		f.rec("pause")
	} else {
		f.rec("resume")
	}
}
func (f *fake) SyncNow(_ context.Context, id int64) error { f.rec("sync"); return nil }
func (f *fake) DiscoverNow(_ context.Context, a string) error {
	f.rec("discover:" + a)
	if a == "missing" {
		return errors.New("unknown")
	}
	return nil
}
func (f *fake) CleanupNow(context.Context, int64) (cleanup.Report, error) {
	f.rec("cleanup")
	if f.cleanupErr != nil {
		return cleanup.Report{}, f.cleanupErr
	}
	return cleanup.Report{Items: []cleanup.Item{{Branch: "a", Outcome: cleanup.Deleted}, {Branch: "b", Outcome: cleanup.Skipped}}}, nil
}
func (f *fake) Restore(_ context.Context, _ int64, b string) error {
	f.rec("restore:" + b)
	return f.restoreEr
}
func (f *fake) ConfigReadOnly() string          { return f.readOnly }
func (f *fake) Config() (string, string, error) { return "[general]\n# " + xss + "\n", "v1", nil }
func (f *fake) SaveConfig(_ context.Context, text, ver string) error {
	f.rec("save:" + ver + ":" + strings.TrimSpace(text))
	return f.saveErr
}
func (f *fake) Bundle(context.Context) ([]byte, error) { return []byte(`{"ok":true}`), nil }

type env struct {
	t   testing.TB
	s   *Server
	b   *fake
	clk *clock.Fake
	h   http.Handler
}

func newEnv(t testing.TB) *env {
	t.Helper()
	b := &fake{report: cleanup.Report{Mode: "dry-run", Items: []cleanup.Item{{Branch: "feat/" + xss, SHA: "abcdef0123456789", Outcome: cleanup.WouldDelete, Reason: cleanup.DeleteMerged}}}}
	clk := clock.NewFake(time.Now())
	s := &Server{Backend: b, Clock: clk, Version: "1.2.3"}
	if err := s.Prepare(addr); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, s: s, b: b, clk: clk, h: s.Handler()}
}

type reqOpt func(*http.Request)

func hdr(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func cookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	}
}

func (e *env) do(method, path string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequestWithContext(e.t.Context(), method, path, body)
	r.Host = addr
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// login performs the full CLI flow and returns the session cookie and CSRF token.
func (e *env) login() (*http.Cookie, string) {
	e.t.Helper()
	w := e.do(http.MethodPost, "/api/login-url", nil, hdr("Authorization", "Bearer "+e.s.Control()))
	if w.Code != 200 {
		e.t.Fatalf("login-url: %d %s", w.Code, w.Body)
	}
	var out struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	u, _ := url.Parse(out.URL)
	w = e.do(http.MethodGet, u.RequestURI(), nil)
	if w.Code != http.StatusSeeOther {
		e.t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var c *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == cookieName {
			c = ck
		}
	}
	if c == nil {
		e.t.Fatal("no session cookie")
	}
	csrf, ok := e.s.auth.lookup(c.Value)
	if !ok {
		e.t.Fatal("session not registered")
	}
	return c, csrf
}

func TestLogin_OneTimeCode_Flags_AndNoCodeInRedirect(t *testing.T) {
	e := newEnv(t)
	code := e.s.auth.mintCode()
	w := e.do(http.MethodGet, "/login?code="+code, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
	var c *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == cookieName {
			c = ck
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || len(c.Value) < 40 {
		t.Fatalf("cookie=%+v", c)
	}
	if w := e.do(http.MethodGet, "/login?code="+code, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("a code must work exactly once, got %d", w.Code)
	}
}

func TestLogin_CodeExpiresAfter60Seconds(t *testing.T) {
	e := newEnv(t)
	code := e.s.auth.mintCode()
	e.clk.Advance(61 * time.Second)
	if w := e.do(http.MethodGet, "/login?code="+code, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired code accepted: %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/login?code=garbage", nil); w.Code != http.StatusUnauthorized {
		t.Fatal("garbage code accepted")
	}
	if w := e.do(http.MethodGet, "/login", nil); w.Code != http.StatusUnauthorized {
		t.Fatal("missing code accepted")
	}
}

func TestSession_ExpiresAfter12Hours_AndLogoutEndsIt(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	if w := e.do(http.MethodGet, "/", nil, cookie(c)); w.Code != 200 {
		t.Fatalf("fresh session: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/logout", url.Values{"csrf": {csrf}}, cookie(c)); w.Code != 200 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/", nil, cookie(c)); w.Code != http.StatusUnauthorized {
		t.Fatal("session survived logout")
	}
	c2, _ := e.login()
	e.clk.Advance(13 * time.Hour)
	if w := e.do(http.MethodGet, "/", nil, cookie(c2)); w.Code != http.StatusUnauthorized {
		t.Fatal("session survived its TTL")
	}
}

func TestEveryPage_RequiresSession(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/fragments/dashboard", "/repos/1", "/accounts", "/cleanup", "/audit", "/config", "/debug", "/debug/bundle.json"} {
		if w := e.do(http.MethodGet, p, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s => %d", p, w.Code)
		}
	}
	for _, p := range []string{"/repos/1/sync", "/repos/1/cleanup", "/repos/1/restore", "/accounts/gh/discover", "/config", "/logout"} {
		if w := e.do(http.MethodPost, p, url.Values{"csrf": {"x"}}); w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s => %d", p, w.Code)
		}
	}
	if e.b.called("sync") || e.b.called("cleanup") {
		t.Fatal("backend reached without a session")
	}
}

func TestHostHeader_BlocksDNSRebinding(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	for _, h := range []string{"evil.example", "evil.example:7878", "127.0.0.1", "127.0.0.1:9999", "localhost", "[::1]:7878"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.Host = h
		r.AddCookie(c)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Host %q => %d", h, w.Code)
		}
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Host = "localhost:7878"
	r.AddCookie(c)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("localhost with the right port must work, got %d", w.Code)
	}
}

func TestCSRF_Origin_FetchSite(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	post := func(form url.Values, opts ...reqOpt) int {
		return e.do(http.MethodPost, "/repos/1/sync", form, append([]reqOpt{cookie(c)}, opts...)...).Code
	}
	if got := post(url.Values{}); got != 403 {
		t.Errorf("missing token => %d", got)
	}
	if got := post(url.Values{"csrf": {"wrong"}}); got != 403 {
		t.Errorf("wrong token => %d", got)
	}
	if got := post(url.Values{"csrf": {csrf}}, hdr("Origin", "http://evil.example")); got != 403 {
		t.Errorf("foreign origin => %d", got)
	}
	if got := post(url.Values{"csrf": {csrf}}, hdr("Origin", "http://127.0.0.1:7878"), hdr("Sec-Fetch-Site", "cross-site")); got != 403 {
		t.Errorf("cross-site fetch => %d", got)
	}
	if e.b.called("sync") {
		t.Fatal("a rejected request reached the backend")
	}
	if got := post(url.Values{"csrf": {csrf}}, hdr("Origin", "http://127.0.0.1:7878"), hdr("Sec-Fetch-Site", "same-origin")); got != http.StatusSeeOther {
		t.Errorf("legitimate request => %d", got)
	}
	if !e.b.called("sync") {
		t.Fatal("backend not called")
	}
}

func TestSecurityHeaders_OnEveryResponse(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	for _, p := range []string{"/", "/nope", "/static/app.js", "/static/app.css", "/repos/1"} {
		w := e.do(http.MethodGet, p, nil, cookie(c))
		h := w.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q: %s", p, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http") {
			t.Errorf("%s: CSP too lax: %s", p, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: missing hardening headers: %v", p, h)
		}
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS must never be enabled", p)
		}
		if !strings.HasPrefix(p, "/static/") && h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: dynamic pages must not be cached", p)
		}
	}
}

func TestControlAPI_RequiresToken(t *testing.T) {
	e := newEnv(t)
	for _, auth := range []string{"", "Bearer wrong", "Basic abc", "Bearer "} {
		var opts []reqOpt
		if auth != "" {
			opts = append(opts, hdr("Authorization", auth))
		}
		if w := e.do(http.MethodPost, "/api/login-url", nil, opts...); w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q => %d", auth, w.Code)
		}
	}
	if w := e.do(http.MethodGet, "/api/login-url", nil, hdr("Authorization", "Bearer "+e.s.Control())); w.Code == 200 {
		t.Error("GET must not mint codes")
	}
}

func TestPages_RenderAndEscapeHostileData(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	// page -> {strings it must contain, whether it displays a hostile value that must appear escaped, whether it has forms}
	type want struct {
		contains []string
		hostile  bool
		forms    bool
	}
	pages := map[string]want{
		"/":                    {[]string{"Dashboard", "acme/", "needs attention", "up to date", "api.github.com", "sync-failed"}, true, true},
		"/repos/1":             {[]string{"Branch cleanup", "would-delete", "Delete safe branches now", "Restore", "Sync history", "merged-into-default"}, true, true},
		"/repos/2":             {[]string{"needs attention", "boom"}, true, true},
		"/accounts":            {[]string{"auth-failed", "octo", "broad scope", "API rate limits"}, true, true},
		"/cleanup":             {[]string{"Cleanup review", "dry-run", "would-delete"}, true, false},
		"/audit":               {[]string{"Audit journal", "deleted", "merged-into-default", "abcdef0123"}, true, false},
		"/config":              {[]string{"Validate and save", "textarea"}, true, true},
		"/debug":               {[]string{"git fetch --prune", "2.50.0", "Download diagnostics bundle", "Restart daemon", "Stop daemon", "/daemon/restart"}, true, true},
		"/fragments/dashboard": {[]string{"Repositories", "Remote hosts"}, true, true},
	}
	for p, w := range pages {
		resp := e.do(http.MethodGet, p, nil, cookie(c))
		if resp.Code != 200 {
			t.Errorf("%s => %d", p, resp.Code)
			continue
		}
		body := resp.Body.String()
		if strings.Contains(body, xss) || strings.Contains(body, "<b>api</b>") {
			t.Errorf("%s: unescaped hostile markup", p)
		}
		if w.hostile && !strings.Contains(body, "&lt;script&gt;") && !strings.Contains(body, "&lt;b&gt;") {
			t.Errorf("%s: hostile value neither escaped nor shown", p)
		}
		if w.forms && !strings.Contains(body, `name="csrf"`) {
			t.Errorf("%s: forms must carry a csrf token", p)
		}
		for _, s := range w.contains {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", p, s)
			}
		}
	}
}

func TestFragment_IsBareHTML(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	body := e.do(http.MethodGet, "/fragments/dashboard", nil, cookie(c)).Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "<title>") || !strings.Contains(body, "<table") {
		t.Fatalf("fragment should be a bare block: %.200s", body)
	}
}

func TestActions_SyncCleanupRestore_Redirects(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	form := func(kv ...string) url.Values {
		v := url.Values{"csrf": {csrf}}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	if w := e.do(http.MethodPost, "/repos/1/sync", form("next", "/"), cookie(c)); w.Code != 303 || w.Header().Get("Location") != "/?m=sync-queued" {
		t.Errorf("sync: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(http.MethodPost, "/repos/1/sync", form("next", "https://evil.example/"), cookie(c)); w.Header().Get("Location") != "/repos/1?m=sync-queued" {
		t.Errorf("open redirect: %q", w.Header().Get("Location"))
	}
	if w := e.do(http.MethodPost, "/repos/1/sync", form("next", "//evil.example"), cookie(c)); !strings.HasPrefix(w.Header().Get("Location"), "/repos/1") {
		t.Errorf("protocol-relative redirect: %q", w.Header().Get("Location"))
	}
	if w := e.do(http.MethodPost, "/repos/1/cleanup", form(), cookie(c)); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "n=1") || !strings.Contains(w.Header().Get("Location"), "cleanup-done") {
		t.Errorf("cleanup: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(http.MethodPost, "/repos/1/restore", form("branch", "feat/x"), cookie(c)); w.Code != 303 || !e.b.called("restore:feat/x") {
		t.Errorf("restore: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/repos/1/restore", form(), cookie(c)); w.Code != 400 {
		t.Errorf("restore without branch: %d", w.Code)
	}
	e.b.restoreEr = errors.New("branch exists")
	if w := e.do(http.MethodPost, "/repos/1/restore", form("branch", "x"), cookie(c)); w.Code != 409 || !strings.Contains(w.Body.String(), "branch exists") {
		t.Errorf("restore conflict: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/accounts/gh/discover", form(), cookie(c)); w.Code != 303 || !e.b.called("discover:gh") {
		t.Errorf("discover: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/accounts/missing/discover", form(), cookie(c)); w.Code != 404 {
		t.Errorf("unknown account: %d", w.Code)
	}
	for _, p := range []string{"/repos/abc", "/repos/0", "/repos/-1", "/repos/999"} {
		if w := e.do(http.MethodGet, p, nil, cookie(c)); w.Code != 404 {
			t.Errorf("GET %s => %d", p, w.Code)
		}
	}
}

func TestConfig_SaveValidationConflictAndSuccess(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	e.b.saveErr = errors.New("general.interval 1m0s is below the 5m0s minimum\ncleanup.mode \"x\" must be off, dry-run or auto")
	w := e.do(http.MethodPost, "/config", url.Values{"csrf": {csrf}, "version": {"v1"}, "text": {"[general]\ninterval=\"1m\"\r\n"}}, cookie(c))
	body := w.Body.String()
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "below the 5m0s minimum") || !strings.Contains(body, "must be off") || !strings.Contains(body, `interval=&#34;1m&#34;`) {
		t.Fatalf("code=%d body=%s", w.Code, body)
	}
	if !e.b.called(`save:v1:[general]` + "\n" + `interval="1m"`) {
		t.Fatalf("calls=%v (CRLF must be normalised)", e.b.calls)
	}
	e.b.saveErr = nil
	if w := e.do(http.MethodPost, "/config", url.Values{"csrf": {csrf}, "version": {"v1"}, "text": {"[general]"}}, cookie(c)); w.Code != 303 || w.Header().Get("Location") != "/config?m=config-saved" {
		t.Fatalf("save: %d %q", w.Code, w.Header().Get("Location"))
	}
	huge := strings.Repeat("a", maxConfigSize+1)
	if w := e.do(http.MethodPost, "/config", url.Values{"csrf": {csrf}, "text": {huge}}, cookie(c)); w.Code < 400 {
		t.Fatalf("oversized config accepted: %d", w.Code)
	}
}

func TestAudit_FilterAndBounds(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	body := e.do(http.MethodGet, "/audit?q=restored", nil, cookie(c)).Body.String()
	if !strings.Contains(body, "restored") || strings.Contains(body, "merged-into-default") {
		t.Fatalf("filter ineffective: %s", body)
	}
	if w := e.do(http.MethodGet, "/audit?n=999999&q=%00", nil, cookie(c)); w.Code != 200 {
		t.Fatalf("hostile params => %d", w.Code)
	}
}

func TestBundle_DownloadHeaders(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	w := e.do(http.MethodGet, "/debug/bundle.json", nil, cookie(c))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers=%v", w.Header())
	}
}

func TestStatic_AssetsServed_NoExternalReferences(t *testing.T) {
	e := newEnv(t)
	js := e.do(http.MethodGet, "/static/app.js", nil)
	css := e.do(http.MethodGet, "/static/app.css", nil)
	if js.Code != 200 || !strings.Contains(js.Header().Get("Content-Type"), "javascript") || css.Code != 200 || !strings.Contains(css.Header().Get("Content-Type"), "css") {
		t.Fatalf("js=%d %q css=%d %q", js.Code, js.Header().Get("Content-Type"), css.Code, css.Header().Get("Content-Type"))
	}
	c, _ := e.login()
	page := e.do(http.MethodGet, "/", nil, cookie(c)).Body.String()
	for _, bad := range []string{"http://", "https://", "<style", " onclick=", " onsubmit=", "javascript:"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains %q (external/inline content breaks the CSP)", bad)
		}
	}
}

func TestListen_PrefersPortFallsBackAndStaysLoopback(t *testing.T) {
	first, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	port := first.Addr().(*net.TCPAddr).Port
	second, err := Listen(port) // taken: must fall back, not fail
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	for _, l := range []net.Listener{first, second} {
		if !l.Addr().(*net.TCPAddr).IP.IsLoopback() {
			t.Fatalf("listener not loopback: %v", l.Addr())
		}
	}
	if second.Addr().(*net.TCPAddr).Port == port {
		t.Fatal("fell back to the same port?")
	}
}

func TestServe_WritesPrivateRuntimeFile_EndToEnd(t *testing.T) {
	b := &fake{}
	s := &Server{Backend: b, Version: "9.9.9"}
	l, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	rt := filepath.Join(t.TempDir(), "run", "ui.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l, rt) }()

	var rf RuntimeFile
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(rt); err == nil && json.Unmarshal(data, &rf) == nil && rf.Control != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rf.Addr != l.Addr().String() || rf.Version != "9.9.9" || rf.PID != os.Getpid() {
		t.Fatalf("runtime file=%+v", rf)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(rt); st.Mode().Perm() != 0o600 {
			t.Fatalf("runtime file mode %v", st.Mode().Perm())
		}
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+rf.Addr+"/api/login-url", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+rf.Control)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login-url: %v %v", err, resp)
	}
	_ = resp.Body.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rt); err == nil {
		t.Fatal("runtime file must be removed on shutdown")
	}
}

func TestAuth_BoundedMemory(t *testing.T) {
	a := newAuth(clock.NewFake(time.Now()))
	for range 100 {
		a.mintCode()
	}
	if len(a.codes) > maxCodes {
		t.Fatalf("codes=%d", len(a.codes))
	}
}

func TestCleanupNow_BusyRepo_Is409NotAnError(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	e.b.cleanupErr = gitx.ErrLocked
	w := e.do(http.MethodPost, "/repos/1/cleanup", url.Values{"csrf": {csrf}}, cookie(c))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "being synced") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
}

func TestMachineAPI_RequiresControlToken_AndNeverCookies(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login() // a browser session must NOT unlock the machine API
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/status"}, {http.MethodPost, "/api/sync-all"}, {http.MethodPost, "/api/pause"}, {http.MethodPost, "/api/resume"},
	} {
		if w := e.do(tc.method, tc.path, url.Values{"csrf": {csrf}}, cookie(c)); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a session cookie => %d", tc.method, tc.path, w.Code)
		}
		if w := e.do(tc.method, tc.path, nil, hdr("Authorization", "Bearer wrong")); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token => %d", tc.method, tc.path, w.Code)
		}
	}
	if e.b.called("sync-all") || e.b.called("pause") {
		t.Fatal("backend reached without the control token")
	}
}

func TestMachineAPI_StatusAndActions(t *testing.T) {
	e := newEnv(t)
	auth := hdr("Authorization", "Bearer "+e.s.Control())
	w := e.do(http.MethodGet, "/api/status", nil, auth)
	var st Status
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Repos != 3 || st.NeedAttention != 1 {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	for path, call := range map[string]string{"/api/sync-all": "sync-all", "/api/pause": "pause", "/api/resume": "resume"} {
		if w := e.do(http.MethodPost, path, nil, auth); w.Code != http.StatusNoContent || !e.b.called(call) {
			t.Errorf("%s => %d (called=%v)", path, w.Code, e.b.called(call))
		}
	}
	// foreign origins cannot drive it even with a leaked token (browsers can't set Authorization cross-origin anyway)
	if w := e.do(http.MethodPost, "/api/pause", nil, auth, hdr("Origin", "http://evil.example")); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin => %d", w.Code)
	}
}

func TestConfig_ReadOnly_ShownAndSaveRefused(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	e.b.readOnly = "managed by Home Manager"
	body := e.do(http.MethodGet, "/config", nil, cookie(c)).Body.String()
	if !strings.Contains(body, "Read-only: managed by Home Manager") || !strings.Contains(body, " readonly") || strings.Contains(body, "Validate and save") {
		t.Fatalf("page must explain and disable editing:\n%s", body)
	}
	w := e.do(http.MethodPost, "/config", url.Values{"csrf": {csrf}, "version": {"v1"}, "text": {"[general]"}}, cookie(c))
	if w.Code != http.StatusForbidden {
		t.Fatalf("save must be refused, got %d", w.Code)
	}
	for _, call := range e.b.calls {
		if strings.HasPrefix(call, "save:") {
			t.Fatal("backend save reached despite read-only config")
		}
	}
}

func FuzzUI_HostileRequests_NeverPanicOrReachBackendUnauthenticated(f *testing.F) {
	e := newEnv(f)
	for _, s := range []string{"/", "/repos/1", "/repos/../../etc/passwd", "/static/../server.go", "/login?code=%00", "/audit?q=%ff&n=-1", "/repos/99999999999999999999"} {
		f.Add(s, "GET")
		f.Add(s, "POST")
	}
	f.Fuzz(func(t *testing.T, path, method string) {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \r\n\x00") || (method != "GET" && method != "POST") {
			return
		}
		u, err := url.ParseRequestURI(path)
		if err != nil {
			return
		}
		r := httptest.NewRequestWithContext(context.Background(), method, u.String(), nil)
		r.Host = addr
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if strings.HasPrefix(u.Path, "/static/") || u.Path == "/login" || u.Path == "/api/login-url" {
			return
		}
		if w.Code == 200 && (u.Path == "/" || strings.HasPrefix(u.Path, "/repos") || strings.HasPrefix(u.Path, "/config") || strings.HasPrefix(u.Path, "/audit")) {
			t.Fatalf("%s %s answered 200 without a session", method, path)
		}
		if len(e.b.calls) != 0 {
			t.Fatalf("backend reached without a session: %v", e.b.calls)
		}
	})
}

// Real browsers send `Origin: null` on form POSTs when the page's Referrer-Policy is no-referrer (Fetch spec,
// "serializing a request origin"), together with Sec-Fetch-Site: same-origin. That must be accepted.
func TestCSRF_RealBrowserHeaders_FormPost_IsAccepted(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	form := url.Values{"csrf": {csrf}}
	post := func(opts ...reqOpt) int {
		return e.do(http.MethodPost, "/repos/1/sync", form, append([]reqOpt{cookie(c)}, opts...)...).Code
	}
	if got := post(hdr("Origin", "null"), hdr("Sec-Fetch-Site", "same-origin"), hdr("Sec-Fetch-Mode", "navigate")); got != http.StatusSeeOther {
		t.Fatalf("Chrome/Firefox form POST (Origin: null, same-origin) => %d, want 303", got)
	}
	if got := post(hdr("Origin", "http://127.0.0.1:7878"), hdr("Sec-Fetch-Site", "same-origin")); got != http.StatusSeeOther {
		t.Fatalf("explicit same origin => %d", got)
	}
	if got := post(hdr("Origin", "null"), hdr("Sec-Fetch-Site", "cross-site")); got != http.StatusForbidden {
		t.Fatalf("a cross-site POST must still be refused even with Origin: null => %d", got)
	}
	if got := post(hdr("Origin", "null"), hdr("Sec-Fetch-Site", "same-site")); got != http.StatusForbidden {
		t.Fatalf("same-site (other port/subdomain) is not same-origin => %d", got)
	}
	if got := post(hdr("Origin", "null")); got != http.StatusForbidden {
		t.Fatalf("Origin: null without Sec-Fetch-Site (old browser or sandboxed iframe) must be refused => %d", got)
	}
	if got := post(); got != http.StatusSeeOther {
		t.Fatalf("non-browser client without Origin headers keeps working (CSRF token still required) => %d", got)
	}
	if rp := e.do(http.MethodGet, "/", nil, cookie(c)).Header().Get("Referrer-Policy"); rp != "same-origin" {
		t.Fatalf("Referrer-Policy=%q", rp)
	}
}

func TestDaemonControl_UIAndMachineAPI(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	form := url.Values{"csrf": {csrf}}
	if w := e.do(http.MethodPost, "/daemon/restart", form, cookie(c)); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "sign in again") || !e.b.called("restart") {
		t.Fatalf("restart: %d %s", w.Code, w.Body)
	}
	if w := e.do(http.MethodPost, "/daemon/stop", form, cookie(c)); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "repo-keeper start") || !e.b.called("shutdown") {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if w := e.do(http.MethodPost, "/daemon/stop", url.Values{}, cookie(c)); w.Code != http.StatusForbidden {
		t.Fatalf("stop without CSRF token must be refused: %d", w.Code)
	}
	e2 := newEnv(t)
	auth := hdr("Authorization", "Bearer "+e2.s.Control())
	if w := e2.do(http.MethodPost, "/api/restart", nil, auth); w.Code != http.StatusAccepted || !e2.b.called("restart") {
		t.Fatalf("api restart: %d", w.Code)
	}
	if w := e2.do(http.MethodPost, "/api/shutdown", nil, auth); w.Code != http.StatusAccepted || !e2.b.called("shutdown") {
		t.Fatalf("api shutdown: %d", w.Code)
	}
	for _, p := range []string{"/api/shutdown", "/api/restart"} {
		if w := e2.do(http.MethodPost, p, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without the control token => %d", p, w.Code)
		}
	}
	if w := e.do(http.MethodPost, "/api/shutdown", form, cookie(c)); w.Code != http.StatusUnauthorized {
		t.Fatalf("a browser session must not unlock the machine API: %d", w.Code)
	}
}
