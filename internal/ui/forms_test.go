// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSettings_Save_PassesEveryFieldAndRedirects(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	form := url.Values{"csrf": {csrf}, "root": {"/data/clones"}, "interval": {"1h"}, "concurrency": {"6"}, "per_host": {"3"},
		"quiet_hours": {"23:00-07:00"}, "all_branches": {"on"}, "secrets": {"file"}, "secrets_file": {"/s/secrets.enc"},
		"cleanup_mode": {"auto"}, "min_age": {"14d"}, "protected": {"main\ndevelop"}, "allow_never_pushed": {"on"}, "ui_enabled": {"on"}, "ui_port": {"9000"}}
	w := e.do(http.MethodPost, "/settings", form, cookie(c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings?m=settings-saved" {
		t.Fatalf("code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
	got := e.b.m.settings[0]
	want := SettingsForm{Root: "/data/clones", Interval: "1h", Concurrency: "6", PerHost: "3", QuietHours: "23:00-07:00", AllBranches: true,
		Secrets: "file", SecretsFile: "/s/secrets.enc", CleanupMode: "auto", MinAge: "14d", Protected: "main\ndevelop", AllowNeverPushed: true, UIEnabled: true, UIPort: "9000"}
	if got != want {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	// unticked checkboxes are false, not "keep the old value"
	e.b.m.settings = nil
	_ = e.post("/settings", url.Values{"root": {"/r"}, "interval": {"1h"}}, c, csrf)
	if g := e.b.m.settings[0]; g.AllBranches || g.AllowNeverPushed || g.UIEnabled {
		t.Fatalf("unticked boxes must be false: %+v", g)
	}
}

func TestSettings_RestartNeeded_IsSaid(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	e.b.m.restart = true
	w := e.do(http.MethodPost, "/settings", url.Values{"csrf": {csrf}, "root": {"/r"}}, cookie(c))
	if w.Header().Get("Location") != "/settings?m=settings-saved-restart" {
		t.Fatalf("loc=%q", w.Header().Get("Location"))
	}
	body := e.do(http.MethodGet, w.Header().Get("Location"), nil, cookie(c)).Body.String()
	if !has(body, "after a restart", "Restart daemon") {
		t.Fatalf("restart hint missing:\n%.600s", body)
	}
}

func TestSettings_ValidationProblems_ShownWithTypedValuesKept(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	e.b.m.err = Invalid("Sync interval: invalid duration \"soon\"", "Parallel syncs must be a whole number")
	w := e.do(http.MethodPost, "/settings", url.Values{"csrf": {csrf}, "root": {"/typed/root"}, "interval": {"soon"}, "concurrency": {"x"}}, cookie(c))
	body := w.Body.String()
	if w.Code != http.StatusUnprocessableEntity || !has(body, "invalid duration", "must be a whole number", `value="/typed/root"`, `value="soon"`) {
		t.Fatalf("code=%d\n%.900s", w.Code, body)
	}
}

func TestSettings_UnexpectedFailure_IsAnErrorPageNotAForm(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	e.b.m.err = errBoom
	if w := e.do(http.MethodPost, "/settings", url.Values{"csrf": {csrf}}, cookie(c)); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "exploded") {
		t.Fatalf("code=%d (internal errors must not leak details)", w.Code)
	}
}

func TestSettings_ReadOnly_DisablesEverything(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	e.b.m.view.ReadOnly = "managed by Home Manager"
	body := e.do(http.MethodGet, "/settings", nil, cookie(c)).Body.String()
	if !has(body, "Read-only: managed by Home Manager", "disabled") || strings.Contains(body, "Save settings") || strings.Contains(body, `name="path" type="text"`) {
		t.Fatalf("read-only config must not offer editing:\n%.800s", body)
	}
}

func TestRepos_AddRemove(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	if w := e.do(http.MethodPost, "/settings/repos", url.Values{"csrf": {csrf}, "path": {"/home/u/new"}, "cleanup_mode": {"auto"}}, cookie(c)); w.Code != http.StatusSeeOther || e.b.m.repos[0].Path != "/home/u/new" || e.b.m.repos[0].CleanupMode != "auto" {
		t.Fatalf("add: %d %+v", w.Code, e.b.m.repos)
	}
	if w := e.do(http.MethodPost, "/settings/repos/remove", url.Values{"csrf": {csrf}, "path": {"/home/u/dotfiles"}}, cookie(c)); w.Code != http.StatusSeeOther || e.b.m.removedRepo[0] != "/home/u/dotfiles" {
		t.Fatalf("remove: %d", w.Code)
	}
	e.b.m.err = Invalid("/nope is not a git repository")
	w := e.do(http.MethodPost, "/settings/repos", url.Values{"csrf": {csrf}, "path": {"/nope"}}, cookie(c))
	if w.Code != http.StatusUnprocessableEntity || !has(w.Body.String(), "not a git repository", `value="/nope"`) {
		t.Fatalf("add invalid: %d\n%.600s", w.Code, w.Body)
	}
}

func TestAccountCreate_Token_IsPassedButNeverEchoed(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	const secret = "ghp_SUPERSECRETtokenValue0123456789"
	form := url.Values{"csrf": {csrf}, "name": {"personal"}, "provider": {"github"}, "auth": {"token"}, "token": {" " + secret + " "},
		"include": {"me/*\nmy-org/**"}, "skip_archived": {"on"}, "root": {"/home/u/code"}}
	if w := e.do(http.MethodPost, "/accounts", form, cookie(c)); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/accounts?m=account-saved" {
		t.Fatalf("create: %d %q", w.Code, w.Header().Get("Location"))
	}
	got := e.b.m.accountsIn[0]
	if !got.New || got.Name != "personal" || got.Token.Reveal() != secret || got.Include != "me/*\nmy-org/**" || !got.SkipArchived || got.Root != "/home/u/code" {
		t.Fatalf("form=%+v", got)
	}

	// a rejected form is shown again WITHOUT the secret anywhere in the response
	e.b.m.err = Invalid("github rejected the credential")
	w := e.do(http.MethodPost, "/accounts", form, cookie(c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "rejected the credential") || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("code=%d secret echoed=%v", w.Code, strings.Contains(w.Body.String(), secret))
	}
	if !strings.Contains(w.Body.String(), `value="personal"`) || !strings.Contains(w.Body.String(), "my-org/**") {
		t.Fatal("the rest of the form must be preserved")
	}
}

func TestAccountUpdate_IdentityComesFromTheURL(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	form := url.Values{"csrf": {csrf}, "name": {"attacker-chosen"}, "provider": {"github"}, "auth": {"keep"}}
	if w := e.do(http.MethodPost, "/accounts/gh", form, cookie(c)); w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d", w.Code)
	}
	if got := e.b.m.accountsIn[0]; got.Name != "gh" || got.New {
		t.Fatalf("a form field must not rename or recreate an account: %+v", got)
	}
}

func TestAccountDevice_Start_Redirects_AndPageStates(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	form := url.Values{"csrf": {csrf}, "name": {"gh2"}, "provider": {"github"}, "auth": {"device"}, "client_id": {"Iv1.0123456789abcdef"}}
	w := e.do(http.MethodPost, "/accounts", form, cookie(c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/devices/dev1" || e.b.m.accountsIn[0].ClientID != "Iv1.0123456789abcdef" {
		t.Fatalf("code=%d loc=%q form=%+v", w.Code, w.Header().Get("Location"), e.b.m.accountsIn)
	}
	page := e.do(http.MethodGet, "/devices/dev1", nil, cookie(c)).Body.String()
	if !has(page, "ABCD-1234", `href="https://github.com/login/device"`, `http-equiv="refresh"`, "Cancel", `rel="noopener noreferrer"`) {
		t.Fatalf("pending page:\n%s", page)
	}
	e.b.m.devices["dev1"] = DeviceView{ID: "dev1", Account: "gh2", State: "done", Login: "octo"}
	if p := e.do(http.MethodGet, "/devices/dev1", nil, cookie(c)).Body.String(); !has(p, "Signed in as", "octo") || strings.Contains(p, "ABCD") {
		t.Fatalf("done page:\n%s", p)
	}
	e.b.m.devices["dev1"] = DeviceView{ID: "dev1", Account: "gh2", State: "failed", Message: "access_denied <script>"}
	if p := e.do(http.MethodGet, "/devices/dev1", nil, cookie(c)).Body.String(); !has(p, "Sign-in failed", "access_denied") || strings.Contains(p, "<script>") {
		t.Fatalf("failed page must show the reason, escaped:\n%s", p)
	}
	if w := e.do(http.MethodGet, "/devices/unknown", nil, cookie(c)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id => %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/devices/dev1/cancel", url.Values{"csrf": {csrf}}, cookie(c)); w.Code != http.StatusSeeOther || !e.b.called("cancel-device") {
		t.Fatalf("cancel: %d", w.Code)
	}
}

func TestAccountRemove_AndDeviceStartErrors(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	if w := e.do(http.MethodPost, "/accounts/gh/remove", url.Values{"csrf": {csrf}, "delete_token": {"on"}}, cookie(c)); w.Code != http.StatusSeeOther || e.b.m.removedAcct[0] != "gh" || !e.b.m.deleteToken {
		t.Fatalf("remove: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/accounts/gh/remove", url.Values{"csrf": {csrf}}, cookie(c)); w.Code != http.StatusSeeOther || e.b.m.deleteToken {
		t.Fatalf("an unticked box keeps the stored token: %d deleteToken=%v", w.Code, e.b.m.deleteToken)
	}
	e.b.m.err = Invalid("A client ID is required.")
	w := e.do(http.MethodPost, "/accounts", url.Values{"csrf": {csrf}, "name": {"x"}, "provider": {"github"}, "auth": {"device"}}, cookie(c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "client ID is required") {
		t.Fatalf("device start error: %d\n%.500s", w.Code, w.Body)
	}
}

func TestEditPage_UnknownAccount_404_AndRawEditorIsGone(t *testing.T) {
	e := newEnv(t)
	c, csrf := e.login()
	if w := e.do(http.MethodGet, "/accounts/ghost/edit", nil, cookie(c)); w.Code != http.StatusNotFound {
		t.Fatalf("code=%d", w.Code)
	}
	if w := e.do(http.MethodPost, "/config", url.Values{"csrf": {csrf}, "text": {"[general]"}}, cookie(c)); w.Code < 400 {
		t.Fatalf("there must be no way to post raw config text any more: %d", w.Code)
	}
	body := e.do(http.MethodGet, "/config", nil, cookie(c)).Body.String()
	if strings.Contains(body, "<textarea") || strings.Contains(body, "<form") && strings.Contains(body, `action="/config"`) {
		t.Fatalf("the configuration file view must be read-only:\n%s", body)
	}
}

func TestMutatingFormRoutes_RequireSessionAndCSRF(t *testing.T) {
	e := newEnv(t)
	c, _ := e.login()
	for _, p := range []string{"/settings", "/settings/repos", "/settings/repos/remove", "/accounts", "/accounts/gh", "/accounts/gh/remove", "/devices/dev1/cancel"} {
		if w := e.do(http.MethodPost, p, url.Values{"x": {"y"}}); w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a session => %d", p, w.Code)
		}
		if w := e.do(http.MethodPost, p, url.Values{"x": {"y"}}, cookie(c)); w.Code != http.StatusForbidden {
			t.Errorf("POST %s without a CSRF token => %d", p, w.Code)
		}
	}
	if len(e.b.calls) != 0 {
		t.Fatalf("backend reached by rejected requests: %v", e.b.calls)
	}
}
