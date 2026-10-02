// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

func client(t *testing.T) *httpx.Client {
	t.Helper()
	c, err := httpx.New(httpx.Config{
		UserAgent: "repo-keeper/test",
		Limiter:   ratelimit.New(ratelimit.Config{Rate: 1000, Burst: 1000}, clock.Real{}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newProv(t *testing.T, h http.Handler) (provider.Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p, err := New(provider.Config{BaseURL: srv.URL, Token: secrets.New("CANARY-token"), HTTP: client(t)})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func TestCheckAuth_ScopesWarningsAndExpiry(t *testing.T) {
	exp := time.Now().Add(3 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 MST")
	p, _ := newProv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer CANARY-token" || r.Header.Get("X-GitHub-Api-Version") == "" || r.UserAgent() != "repo-keeper/test" {
			http.Error(w, "bad headers", 400)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		w.Header().Set("github-authentication-token-expiration", exp)
		_, _ = w.Write([]byte(`{"login":"octo"}`))
	}))
	a, err := p.CheckAuth(t.Context())
	if err != nil || a.Login != "octo" || len(a.Scopes) != 2 || a.Expires.IsZero() {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	if len(a.Warnings) != 2 || !strings.Contains(a.Warnings[0], `"repo"`) || !strings.Contains(a.Warnings[1], "expires") {
		t.Fatalf("warnings=%v", a.Warnings)
	}
}

func TestCheckAuth_FineGrainedToken_NoWarnings(t *testing.T) {
	p, _ := newProv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"login":"octo"}`)) }))
	a, err := p.CheckAuth(t.Context())
	if err != nil || len(a.Warnings) != 0 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
}

func TestCheckAuth_401_IsErrAuthAndNeverEchoesToken(t *testing.T) {
	p, _ := newProv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	_, err := p.CheckAuth(t.Context())
	if !errors.Is(err, provider.ErrAuth) || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("err=%v", err)
	}
}

func TestListRepos_PagingAndMapping(t *testing.T) {
	var srvURL string
	p, srv := newProv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next"`, srvURL))
			_, _ = w.Write([]byte(`[{"id":1,"name":"api","full_name":"acme/api","clone_url":"https://github.com/acme/api.git","ssh_url":"git@github.com:acme/api.git","default_branch":"main","private":true}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"id":2,"name":"old","full_name":"me/old","default_branch":"master","archived":true,"fork":true}]`))
		}
	}))
	srvURL = srv.URL
	repos, err := p.ListRepos(t.Context())
	if err != nil || len(repos) != 2 {
		t.Fatalf("repos=%+v err=%v", repos, err)
	}
	r := repos[0]
	if r.ID != "1" || r.FullName != "acme/api" || r.Namespace[0] != "acme" || r.Name != "api" || !r.Private || r.DefaultBranch != "main" || r.SSHURL == "" {
		t.Fatalf("r=%+v", r)
	}
	if !repos[1].Archived || !repos[1].Fork {
		t.Fatalf("flags lost: %+v", repos[1])
	}
}

func TestListRepos_RefusesPaginationToForeignHost(t *testing.T) {
	var leaked bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = r.Header.Get("Authorization") != "" }))
	defer evil.Close()
	p, _ := newProv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/steal>; rel="next"`, evil.URL))
		_, _ = w.Write([]byte(`[]`))
	}))
	if _, err := p.ListRepos(t.Context()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err=%v", err)
	}
	if leaked {
		t.Fatal("token sent to a foreign host")
	}
}

func TestListRepos_ServerError_Surfaces(t *testing.T) {
	p, _ := newProv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible"}`))
	}))
	_, err := p.ListRepos(t.Context())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("err=%v", err)
	}
}

func TestMergedBranches_OnlyOwnRepoMergedPRs(t *testing.T) {
	p, _ := newProv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/acme/api/pulls") || r.URL.Query().Get("state") != "closed" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[
		 {"merged_at":"2026-01-02T00:00:00Z","head":{"ref":"feat-a","sha":"aaa","repo":{"full_name":"acme/api"}}},
		 {"merged_at":null,"head":{"ref":"feat-b","sha":"bbb","repo":{"full_name":"acme/api"}}},
		 {"merged_at":"2026-01-01T00:00:00Z","head":{"ref":"feat-c","sha":"ccc","repo":{"full_name":"someone/api"}}},
		 {"merged_at":"2026-01-01T00:00:00Z","head":{"ref":"feat-d","sha":"ddd","repo":null}},
		 {"merged_at":"2025-12-01T00:00:00Z","head":{"ref":"feat-a","sha":"old","repo":{"full_name":"ACME/api"}}},
		 {"merged_at":"2025-12-01T00:00:00Z","head":{"ref":"unrelated","sha":"uuu","repo":{"full_name":"acme/api"}}}
		]`))
	}))
	got, err := p.MergedBranches(t.Context(), provider.Repo{FullName: "acme/api"}, []string{"feat-a", "feat-b", "feat-c", "feat-d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["feat-a"] != "aaa" {
		t.Fatalf("got=%v", got)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(provider.Config{}); err == nil {
		t.Fatal("HTTP client required")
	}
	if _, err := New(provider.Config{HTTP: client(t), BaseURL: "::bad"}); err == nil {
		t.Fatal("bad base url")
	}
	p, err := New(provider.Config{HTTP: client(t)})
	if err != nil || p.APIHost() != "api.github.com" {
		t.Fatalf("host=%v err=%v", p, err)
	}
	if reg, err := provider.New(provider.GitHub, provider.Config{HTTP: client(t)}); err != nil || reg.Kind() != provider.GitHub {
		t.Fatalf("registry: %v", err)
	}
}

func deviceServer(t *testing.T, script []string) (*Device, *clock.Fake, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/login/device/code":
			if r.Form.Get("client_id") != "cid" || r.Form.Get("scope") != "" {
				http.Error(w, "bad form", 400)
				return
			}
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		case "/login/oauth/access_token":
			mu.Lock()
			defer mu.Unlock()
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "dc" {
				http.Error(w, "bad grant", 400)
				return
			}
			resp := script[calls]
			calls++
			_, _ = w.Write([]byte(resp))
		}
	}))
	t.Cleanup(srv.Close)
	clk := clock.NewFake(time.Now())
	return &Device{HTTP: client(t), Clock: clk, WebBase: srv.URL, ClientID: "cid"}, clk, &calls
}

func TestDevice_PendingSlowDownThenToken(t *testing.T) {
	d, clk, calls := deviceServer(t, []string{
		`{"error":"authorization_pending"}`, `{"error":"slow_down","interval":10}`, `{"access_token":"gho_CANARY0123456789abcdef"}`,
	})
	dc, err := d.Start(t.Context())
	if err != nil || dc.UserCode != "ABCD-1234" || dc.Interval != 5 {
		t.Fatalf("dc=%+v err=%v", dc, err)
	}
	type res struct {
		tok secrets.Token
		err error
	}
	done := make(chan res, 1)
	go func() { tok, err := d.Poll(t.Context(), dc); done <- res{tok, err} }()

	for _, step := range []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second} {
		if !clk.BlockUntil(1, 2*time.Second) {
			t.Fatal("poller not waiting")
		}
		clk.Advance(step)
		if step == 5*time.Second && *calls == 0 {
			continue
		}
	}
	r := <-done
	if r.err != nil || r.tok.Reveal() != "gho_CANARY0123456789abcdef" {
		t.Fatalf("res=%+v", r)
	}
}

func TestDevice_TerminalErrors(t *testing.T) {
	for name, tc := range map[string]struct{ resp, want string }{
		"denied":  {`{"error":"access_denied"}`, "denied"},
		"expired": {`{"error":"expired_token"}`, "expired"},
		"other":   {`{"error":"incorrect_client_credentials","error_description":"nope"}`, "incorrect_client_credentials"},
	} {
		t.Run(name, func(t *testing.T) {
			d, clk, _ := deviceServer(t, []string{tc.resp})
			dc, _ := d.Start(t.Context())
			done := make(chan error, 1)
			go func() { _, err := d.Poll(context.Background(), dc); done <- err }()
			clk.BlockUntil(1, 2*time.Second)
			clk.Advance(5 * time.Second)
			if err := <-done; err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDevice_Start_RejectsIncompleteResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	d := &Device{HTTP: client(t), Clock: clock.Real{}, WebBase: srv.URL, ClientID: "x"}
	if _, err := d.Start(t.Context()); err == nil {
		t.Fatal("expected error")
	}
}
