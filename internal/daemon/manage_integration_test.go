// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/ui"
)

const baseCfg = `# my hand-written notes
[general]
root = %q
interval = "30m"

# cleanup stays on dry-run while I test
[cleanup]
mode = "dry-run"
min_age = "0s"
`

type managed struct {
	*rig
	b    *browserSession
	gh   *fakeGH
	srv  *httptest.Server
	root string
}

func startManaged(t *testing.T, cfgFmt string) *managed {
	t.Helper()
	e := gitxtest.New(t)
	gh := &fakeGH{}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	gh.set(ghRepo("acme/api", e.Origin, false), ghRepo("acme/old", e.Origin, true), ghRepo("other/x", e.Origin, false))
	root := filepath.Join(t.TempDir(), "code")
	cfg := cfgFmt
	if strings.Contains(cfgFmt, "%q") {
		cfg = strings.Replace(cfgFmt, "%q", strconv.Quote(root), 1)
	}
	r, b := startWithUI(t, e, cfg)
	// credential checks queue behind the per-host rate limiter, which runs on the fake clock
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				r.clk.Advance(time.Second)
			}
		}
	}()
	t.Cleanup(func() { close(stop) })
	return &managed{rig: r, b: b, gh: gh, srv: srv, root: root}
}

func (m *managed) cfgText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(m.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func accountForm(srv *httptest.Server, name, auth string, extra url.Values) url.Values {
	v := url.Values{"name": {name}, "provider": {"github"}, "base_url": {srv.URL}, "auth": {auth}, "include": {"acme/*"},
		"skip_archived": {"on"}, "root": {""}}
	for k, vs := range extra {
		v[k] = vs
	}
	return v
}

func (m *managed) waitAccountOK(t *testing.T, name string) {
	t.Helper()
	m.waitFor(t, "account "+name+" listed", func() bool {
		as, _ := m.d.Store.ListAccounts(context.Background())
		for _, a := range as {
			if a.Name == name && a.Status == "ok" && a.RepoCount > 0 {
				return true
			}
		}
		return false
	})
}

func TestManage_AddAccount_Token_VerifiedStoredListedAndNeverLeaked(t *testing.T) {
	m := startManaged(t, baseCfg)
	before := m.cfgText(t)

	// a wrong token is explained, and nothing is saved anywhere
	code, body := m.b.post("/accounts", accountForm(m.srv, "personal", "token", url.Values{"token": {"not-the-token"}}))
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "rejected the credential") || strings.Contains(body, "not-the-token") {
		t.Fatalf("bad token: %d\n%.600s", code, body)
	}
	if m.cfgText(t) != before {
		t.Fatal("a rejected credential must not change the config")
	}
	if _, err := m.secrets.Get("account/personal"); err == nil {
		t.Fatal("a rejected credential must not be stored")
	}
	if code, body = m.b.post("/accounts", accountForm(m.srv, "personal", "token", nil)); code != http.StatusUnprocessableEntity || !strings.Contains(body, "Paste the access token") {
		t.Fatalf("missing token: %d\n%.400s", code, body)
	}

	// the right token: verified, stored in the secret store, written to the config WITHOUT the token, repos listed and cloned
	code, body = m.b.post("/accounts", accountForm(m.srv, "personal", "token", url.Values{"token": {ghToken}}))
	if code != 200 || !strings.Contains(body, "Account saved") {
		t.Fatalf("add: %d\n%.700s", code, body)
	}
	if got, err := m.secrets.Get("account/personal"); err != nil || got.Reveal() != ghToken {
		t.Fatalf("token not stored: %v", err)
	}
	text := m.cfgText(t)
	if !strings.Contains(text, `name = "personal"`) || !strings.Contains(text, `include = ["acme/*"]`) || strings.Contains(text, ghToken) {
		t.Fatalf("config:\n%s", text)
	}
	if !strings.Contains(text, "# my hand-written notes") || !strings.Contains(text, "# cleanup stays on dry-run while I test") {
		t.Fatalf("the user's comments must survive a form edit:\n%s", text)
	}
	m.waitAccountOK(t, "personal")
	m.waitFor(t, "clone", func() bool {
		rs := m.repos(t)
		return len(rs) == 1 && rs[0].LastReason == "cloned"
	})

	// duplicates are refused
	if code, body = m.b.post("/accounts", accountForm(m.srv, "personal", "token", url.Values{"token": {ghToken}})); code != http.StatusUnprocessableEntity || !strings.Contains(body, "already exists") {
		t.Fatalf("duplicate: %d\n%.400s", code, body)
	}

	// the secret is nowhere a user (or a bug report) could find it
	for _, p := range []string{"/", "/accounts", "/settings", "/config", "/debug", "/audit", "/accounts/personal/edit", "/debug/bundle.json"} {
		if _, page := m.b.get(p); strings.Contains(page, ghToken) {
			t.Errorf("token visible on %s", p)
		}
	}
}

func TestManage_EditAndRemoveAccount(t *testing.T) {
	m := startManaged(t, baseCfg)
	m.b.post("/accounts", accountForm(m.srv, "personal", "token", url.Values{"token": {ghToken}}))
	m.waitAccountOK(t, "personal")
	m.waitFor(t, "clone", func() bool { rs := m.repos(t); return len(rs) == 1 && rs[0].LastReason == "cloned" })

	// edit: keep the credential, change what is included; identity cannot be changed through the form
	form := accountForm(m.srv, "someone-else", "keep", url.Values{"include": {"acme/*\nother/*"}, "skip_forks": {"on"}, "discovery_interval": {"12h"}})
	if code, body := m.b.post("/accounts/personal", form); code != 200 || !strings.Contains(body, "Account saved") {
		t.Fatalf("edit: %d\n%.600s", code, body)
	}
	text := m.cfgText(t)
	if strings.Count(text, "[[account]]") != 1 || !strings.Contains(text, `name = "personal"`) || strings.Contains(text, "someone-else") ||
		!strings.Contains(text, "other/*") || !strings.Contains(text, "skip_forks = true") || !strings.Contains(text, `discovery_interval = "12h"`) {
		t.Fatalf("edit not applied cleanly:\n%s", text)
	}
	if got, _ := m.secrets.Get("account/personal"); got.Reveal() != ghToken {
		t.Fatal("'keep' must leave the stored token alone")
	}
	if _, page := m.b.get("/accounts/personal/edit"); !strings.Contains(page, "other/*") || !strings.Contains(page, "Keep the current credential") {
		t.Fatalf("edit form not prefilled:\n%.800s", page)
	}

	// replace the token by pasting a new one (verified first)
	if code, _ := m.b.post("/accounts/personal", accountForm(m.srv, "personal", "token", url.Values{"token": {"wrong"}})); code != http.StatusUnprocessableEntity {
		t.Fatalf("a wrong replacement token must be refused: %d", code)
	}
	if got, _ := m.secrets.Get("account/personal"); got.Reveal() != ghToken {
		t.Fatal("the old token must survive a refused replacement")
	}

	// remove with "forget token": account, token and tracking go; the clone on disk stays
	clone := filepath.Join(m.root, "github", "acme", "api")
	if code, body := m.b.post("/accounts/personal/remove", url.Values{"delete_token": {"on"}}); code != 200 || !strings.Contains(body, "Account removed") {
		t.Fatalf("remove: %d\n%.500s", code, body)
	}
	if strings.Contains(m.cfgText(t), "[[account]]") || !strings.Contains(m.cfgText(t), "# my hand-written notes") {
		t.Fatalf("config after removal:\n%s", m.cfgText(t))
	}
	if _, err := m.secrets.Get("account/personal"); err == nil {
		t.Fatal("the token should have been forgotten")
	}
	m.waitFor(t, "repos deactivated", func() bool { return len(m.repos(t)) == 0 })
	if _, err := os.Stat(filepath.Join(clone, "README.md")); err != nil {
		t.Fatal("removing an account must never delete cloned repositories")
	}
	if code, _ := m.b.post("/accounts/personal/remove", url.Values{}); code != http.StatusConflict {
		t.Fatalf("removing a missing account: %d", code)
	}
}

func TestManage_RemoveAccount_KeepToken_WhenAskedTo(t *testing.T) {
	m := startManaged(t, baseCfg)
	m.b.post("/accounts", accountForm(m.srv, "personal", "token", url.Values{"token": {ghToken}}))
	m.b.post("/accounts/personal/remove", url.Values{}) // box not ticked
	if got, err := m.secrets.Get("account/personal"); err != nil || got.Reveal() != ghToken {
		t.Fatal("without 'forget token' the stored token stays")
	}
}

func (m *managed) completeDevice(t *testing.T, id string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, page := m.b.get("/devices/" + id)
		if strings.Contains(page, "Signed in as") || strings.Contains(page, "Sign-in failed") || strings.Contains(page, "Sign-in cancelled") {
			return page
		}
		m.advance(6 * time.Second) // the poller waits on the daemon's clock
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("device login never finished")
	return ""
}

var deviceID = regexp.MustCompile(`/devices/([0-9a-f]+)`)

func TestManage_DeviceFlow_EndToEnd(t *testing.T) {
	m := startManaged(t, baseCfg)
	m.gh.mu.Lock()
	m.gh.devicePending = 2
	m.gh.mu.Unlock()

	form := accountForm(m.srv, "viadevice", "device", url.Values{"client_id": {"Iv1.0123456789abcdef"}, "web_url": {m.srv.URL}})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, m.b.base+"/accounts", strings.NewReader(func() string { form.Set("csrf", m.b.csrf); return form.Encode() }()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := &http.Client{Jar: m.b.c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: %v %v", err, resp)
	}
	_ = resp.Body.Close()
	id := deviceID.FindStringSubmatch(resp.Header.Get("Location"))[1]

	_, page := m.b.get("/devices/" + id)
	if !strings.Contains(page, "WDJB-MJHT") || !strings.Contains(page, `href="https://github.com/login/device"`) {
		t.Fatalf("the user must be shown the code and link:\n%.800s", page)
	}
	if _, err := m.secrets.Get("account/viadevice"); err == nil || strings.Contains(m.cfgText(t), "viadevice") {
		t.Fatal("nothing may be saved before the user approves")
	}

	done := m.completeDevice(t, id)
	if !strings.Contains(done, "octo") {
		t.Fatalf("done page:\n%.600s", done)
	}
	if got, err := m.secrets.Get("account/viadevice"); err != nil || got.Reveal() != ghToken {
		t.Fatalf("token not stored after approval: %v", err)
	}
	text := m.cfgText(t)
	if !strings.Contains(text, `name = "viadevice"`) || !strings.Contains(text, `oauth_client_id = "Iv1.0123456789abcdef"`) || strings.Contains(text, ghToken) {
		t.Fatalf("config:\n%s", text)
	}
	m.waitAccountOK(t, "viadevice")
}

func TestManage_DeviceFlow_DeniedCancelledAndValidation(t *testing.T) {
	m := startManaged(t, baseCfg)
	base := func(name string) url.Values {
		return accountForm(m.srv, name, "device", url.Values{"client_id": {"Iv1.0123456789abcdef"}, "web_url": {m.srv.URL}})
	}
	start := func(f url.Values) (int, string) {
		f.Set("csrf", m.b.csrf)
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, m.b.base+"/accounts", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		c := &http.Client{Jar: m.b.c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, resp.Header.Get("Location")
	}

	// denied in the browser: failed page with the reason, nothing saved
	m.gh.mu.Lock()
	m.gh.deviceDenied = true
	m.gh.mu.Unlock()
	code, loc := start(base("denied"))
	if code != http.StatusSeeOther {
		t.Fatalf("start: %d", code)
	}
	if page := m.completeDevice(t, deviceID.FindStringSubmatch(loc)[1]); !strings.Contains(page, "denied") {
		t.Fatalf("failed page:\n%.500s", page)
	}
	if strings.Contains(m.cfgText(t), `"denied"`) {
		t.Fatal("a denied sign-in must not create the account")
	}

	// cancelled by the user
	m.gh.mu.Lock()
	m.gh.deviceDenied, m.gh.devicePending = false, 1000
	m.gh.mu.Unlock()
	_, loc = start(base("cancelme"))
	id := deviceID.FindStringSubmatch(loc)[1]
	if code, _ := m.b.post("/devices/"+id+"/cancel", nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	if page := m.completeDevice(t, id); !strings.Contains(page, "cancelled") {
		t.Fatalf("cancelled page:\n%.500s", page)
	}

	// validation: missing client id, wrong platform
	if code, body := m.b.post("/accounts", accountForm(m.srv, "nocid", "device", nil)); code != http.StatusUnprocessableEntity || !strings.Contains(body, "client ID is required") {
		t.Fatalf("no client id: %d\n%.400s", code, body)
	}
	gl := accountForm(m.srv, "lab", "device", url.Values{"client_id": {"Iv1.0123456789abcdef"}, "provider": {"gitlab"}})
	if code, body := m.b.post("/accounts", gl); code != http.StatusUnprocessableEntity || !strings.Contains(body, "only available for GitHub") {
		t.Fatalf("gitlab device: %d\n%.400s", code, body)
	}
}

func TestManage_FirstAccountNeedsACloneFolder_ThenSetsIt(t *testing.T) {
	m := startManaged(t, "# no root yet\n[cleanup]\nmode = \"dry-run\"\n")
	f := accountForm(m.srv, "personal", "token", url.Values{"token": {ghToken}})
	if code, body := m.b.post("/accounts", f); code != http.StatusUnprocessableEntity || !strings.Contains(body, "Choose the folder") {
		t.Fatalf("without a root: %d\n%.500s", code, body)
	}
	f.Set("root", m.root)
	if code, body := m.b.post("/accounts", f); code != 200 || !strings.Contains(body, "Account saved") {
		t.Fatalf("with a root: %d\n%.500s", code, body)
	}
	if text := m.cfgText(t); !strings.Contains(text, "root = ") || !strings.Contains(text, "# no root yet") {
		t.Fatalf("root not written, comment lost?\n%s", text)
	}
	m.waitAccountOK(t, "personal")
}

func TestManage_ReadOnlyConfig_RefusesEveryForm(t *testing.T) {
	e := gitxtest.New(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "managed.toml")
	content := "[general]\nroot = " + strconv.Quote(filepath.Join(dir, "code")) + "\n"
	writeCfg(t, real, content)
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	d := &Daemon{ConfigPath: link, StateDir: filepath.Join(dir, "state"), Runner: e.R, Clock: nowClock{}}
	b := uiBackend{d}
	ctx := context.Background()
	for name, err := range map[string]error{
		"add repo": b.AddRepo(ctx, ui.RepoForm{Path: e.Work}),
		"remove":   b.RemoveRepo(ctx, e.Work),
		"account":  b.SaveAccount(ctx, ui.AccountForm{New: true, Name: "a", Provider: "github", Auth: "file", TokenFile: filepath.Join(dir, "x"), Root: filepath.Join(dir, "code")}),
	} {
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if got, _ := os.ReadFile(real); string(got) != content {
		t.Fatalf("managed file modified: %q", got)
	}
}

func TestSafeLinkURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/login/device": "https://github.com/login/device", "http://127.0.0.1:9/x": "http://127.0.0.1:9/x",
		"javascript:alert(1)": "", "http://example.com/x": "", "data:text/html,x": "", "//evil.example": "", "": "", "https://": "",
	} {
		if got := safeLinkURL(in); got != want {
			t.Errorf("safeLinkURL(%q) = %q, want %q", in, got, want)
		}
	}
}
