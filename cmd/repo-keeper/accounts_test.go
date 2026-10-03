// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

const tok = "ghp_CANARYabcdefghijklmnopqrstuvwxyz0123"

func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		switch r.URL.Path {
		case "/user":
			w.Header().Set("X-OAuth-Scopes", "repo")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "4990")
			w.Header().Set("X-RateLimit-Reset", "4102444800")
			_, _ = w.Write([]byte(`{"login":"octo"}`))
		case "/user/repos":
			_, _ = w.Write([]byte(`[
			 {"id":1,"name":"api","full_name":"acme/api","clone_url":"https://x/acme/api.git","default_branch":"main"},
			 {"id":2,"name":"old","full_name":"acme/old","archived":true},
			 {"id":3,"name":"x","full_name":"other/x"},
			 {"id":4,"name":"have","full_name":"acme/have"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T) (a *app, stdout, stderr *strings.Builder, cfg string, srv *httptest.Server) {
	t.Helper()
	e := gitxtest.New(t)
	var out, errb strings.Builder
	a = &app{out: &out, err: &errb, in: strings.NewReader(""), secrets: &secrets.Mem{}, clock: clock.Real{}}
	a.newRunner = func() (*gitx.Runner, error) { return e.R, nil }
	cfg = filepath.Join(e.Root, "config.toml")
	root := filepath.Join(e.Root, "code")
	_ = os.WriteFile(cfg, []byte("[general]\nroot = "+strconv.Quote(root)+"\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(root, "github", "acme", "have"), 0o750)
	return a, &out, &errb, cfg, fakeGitHub(t)
}

func TestAccounts_Add_TokenStdin_VerifiesStoresAndAppendsStanza(t *testing.T) {
	a, out, errb, cfg, srv := setup(t)
	a.in = strings.NewReader(tok + "\n")
	code := a.run(context.Background(), []string{"accounts", "add", "gh", "--base-url", srv.URL, "--token-stdin", "--include", "acme/*", "--config", cfg})
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out.String(), "authenticated as octo") || !strings.Contains(out.String(), `broad "repo" scope`) {
		t.Fatalf("out=%s", out)
	}
	got, err := a.secrets.Get("account/gh")
	if err != nil || got.Reveal() != tok {
		t.Fatalf("secret not stored: %v", err)
	}
	body, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body), `name = "gh"`) || !strings.Contains(string(body), `include = ["acme/*"]`) || strings.Contains(string(body), tok) {
		t.Fatalf("config=%s", body)
	}
	if code := a.run(context.Background(), []string{"config", "validate", "--config", cfg}); code != 0 {
		t.Fatalf("config invalid after add: %s", errb)
	}
	errb.Reset()
	a.in = strings.NewReader(tok + "\n")
	if code := a.run(context.Background(), []string{"accounts", "add", "gh", "--base-url", srv.URL, "--token-stdin", "--config", cfg}); code != 1 || !strings.Contains(errb.String(), "already exists") {
		t.Fatalf("duplicate must be refused: code=%d err=%s", code, errb)
	}
}

func TestAccounts_Add_RejectedCredential_StoresNothing(t *testing.T) {
	a, _, errb, cfg, srv := setup(t)
	a.in = strings.NewReader("wrong-token\n")
	before, _ := os.ReadFile(cfg)
	if code := a.run(context.Background(), []string{"accounts", "add", "gh", "--base-url", srv.URL, "--token-stdin", "--config", cfg}); code != 1 || !strings.Contains(errb.String(), "rejected") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if _, err := a.secrets.Get("account/gh"); err == nil {
		t.Fatal("a rejected token must not be stored")
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Fatal("config modified despite failure")
	}
	if strings.Contains(errb.String(), "wrong-token") {
		t.Fatal("token echoed")
	}
}

func TestAccounts_Add_TokenFile_NothingInKeychain(t *testing.T) {
	a, _, errb, cfg, srv := setup(t)
	tf := filepath.Join(filepath.Dir(cfg), "tok")
	_ = os.WriteFile(tf, []byte(tok), 0o600)
	if code := a.run(context.Background(), []string{"accounts", "add", "gh", "--base-url", srv.URL, "--token-file", tf, "--config", cfg}); code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if _, err := a.secrets.Get("account/gh"); err == nil {
		t.Fatal("file-based accounts must not copy the token into the keychain")
	}
	if body, _ := os.ReadFile(cfg); !strings.Contains(string(body), "token_file") {
		t.Fatalf("config=%s", body)
	}
}

func TestAccounts_Add_NoSourceOrNoConfig(t *testing.T) {
	a, _, errb, cfg, _ := setup(t)
	if code := a.run(context.Background(), []string{"accounts", "add", "gh", "--config", cfg}); code != 2 || !strings.Contains(errb.String(), "credential source") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	errb.Reset()
	if code := a.run(context.Background(), []string{"accounts", "add", "gh", "--token-stdin", "--config", filepath.Join(t.TempDir(), "none.toml")}); code != 1 || !strings.Contains(errb.String(), "init") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func addAccount(t *testing.T, a *app, cfg string, srv *httptest.Server) {
	t.Helper()
	_ = a.secrets.Set("account/gh", secrets.New(tok))
	f, _ := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("\n[[account]]\nname = \"gh\"\nprovider = \"github\"\nbase_url = \"" + srv.URL + "\"\ninclude = [\"acme/*\"]\n")
	_ = f.Close()
}

func TestAccounts_List_Check_Rm(t *testing.T) {
	a, out, errb, cfg, srv := setup(t)
	addAccount(t, a, cfg, srv)
	ctx := context.Background()

	if code := a.run(ctx, []string{"accounts", "list", "--config", cfg}); code != 0 || !strings.Contains(out.String(), "keychain") || !strings.Contains(out.String(), "yes") {
		t.Fatalf("list: code=%d out=%s", code, out)
	}
	out.Reset()
	if code := a.run(ctx, []string{"accounts", "check", "gh", "--config", cfg}); code != 0 || !strings.Contains(out.String(), "octo") || !strings.Contains(out.String(), "4990/5000") {
		t.Fatalf("check: code=%d out=%s err=%s", code, out, errb)
	}
	if code := a.run(ctx, []string{"accounts", "check", "nope", "--config", cfg}); code != 1 {
		t.Fatalf("unknown account code=%d", code)
	}
	out.Reset()
	if code := a.run(ctx, []string{"accounts", "rm", "gh"}); code != 0 {
		t.Fatalf("rm code=%d", code)
	}
	if code := a.run(ctx, []string{"accounts", "list", "--config", cfg}); code != 0 || !strings.Contains(out.String(), "not stored") {
		t.Fatalf("list after rm: %s", out)
	}
}

func TestDiscover_ShowsPlanWithoutChangingAnything(t *testing.T) {
	a, out, errb, cfg, srv := setup(t)
	addAccount(t, a, cfg, srv)
	if code := a.run(context.Background(), []string{"discover", "--config", cfg}); code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	o := out.String()
	for _, want := range []string{"4 repositories visible", "acme/api", "clone", "acme/old", "skip (archived)", "other/x", "skip (filtered)", "acme/have", "tracked"} {
		if !strings.Contains(o, want) {
			t.Errorf("output lacks %q:\n%s", want, o)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "code", "github", "acme", "api")); err == nil {
		t.Fatal("discover must not create anything")
	}
}

func TestAccounts_Add_GitLab_AndDeviceLoginRefusedForIt(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v4/user" {
			_, _ = w.Write([]byte(`{"username":"gina"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer gl.Close()
	a, out, errb, cfg, _ := setup(t)
	ctx := context.Background()

	if code := a.run(ctx, []string{"accounts", "add", "lab", "--provider", "gitlab", "--device", "--client-id", "x", "--config", cfg}); code != 2 || !strings.Contains(errb.String(), "only available for github") {
		t.Fatalf("device for gitlab: code=%d err=%s", code, errb)
	}
	a.in = strings.NewReader(tok + "\n")
	if code := a.run(ctx, []string{"accounts", "add", "lab", "--provider", "gitlab", "--base-url", gl.URL, "--token-stdin", "--include", "acme/**", "--config", cfg}); code != 0 {
		t.Fatalf("add: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out.String(), "authenticated as gina") {
		t.Fatalf("out=%s", out)
	}
	body, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body), `provider = "gitlab"`) || !strings.Contains(string(body), `include = ["acme/**"]`) {
		t.Fatalf("config=%s", body)
	}
	if code := a.run(ctx, []string{"config", "validate", "--config", cfg}); code != 0 {
		t.Fatalf("config invalid: %s", errb)
	}
}

func TestAccounts_EncryptedFileBackend_EndToEnd(t *testing.T) {
	a, out, errb, cfg, srv := setup(t)
	a.secrets = nil // use the real backend selection
	dir := filepath.Dir(cfg)
	pass := filepath.Join(dir, "pass")
	_ = os.WriteFile(pass, []byte("a long passphrase\n"), 0o600)
	t.Setenv("REPO_KEEPER_PASSPHRASE_FILE", pass)
	encPath := filepath.Join(dir, "secrets.enc")
	body, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(body, []byte("secrets = \"file\"\nsecrets_file = "+strconv.Quote(encPath)+"\n")...), 0o600)
	// "secrets" keys belong to [general], which setup() already opened; the file ends inside it
	a.in = strings.NewReader(tok + "\n")
	ctx := context.Background()
	if code := a.run(ctx, []string{"accounts", "add", "gh", "--base-url", srv.URL, "--token-stdin", "--config", cfg}); code != 0 {
		t.Fatalf("add: code=%d err=%s", code, errb)
	}
	raw, err := os.ReadFile(encPath)
	if err != nil || strings.Contains(string(raw), tok) {
		t.Fatalf("encrypted file missing or leaking: %v", err)
	}
	out.Reset()
	if code := a.run(ctx, []string{"accounts", "check", "gh", "--config", cfg}); code != 0 || !strings.Contains(out.String(), "octo") {
		t.Fatalf("check through the encrypted store: code=%d out=%s err=%s", code, out, errb)
	}
	t.Setenv("REPO_KEEPER_PASSPHRASE_FILE", "")
	errb.Reset()
	if code := a.run(ctx, []string{"accounts", "check", "gh", "--config", cfg}); code != 1 || !strings.Contains(errb.String(), "REPO_KEEPER_PASSPHRASE_FILE") {
		t.Fatalf("without a passphrase: code=%d err=%s", code, errb)
	}
}
