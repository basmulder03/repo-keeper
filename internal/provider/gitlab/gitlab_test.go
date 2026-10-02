// SPDX-License-Identifier: Apache-2.0

package gitlab

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/provider/providertest"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

const good = "glpat-GOODtokenValue0123456789"

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

// fakeGitLab serves a fixture with nested groups over two keyset pages.
func fakeGitLab(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+good || r.UserAgent() != "repo-keeper/test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
			return
		}
		switch r.URL.EscapedPath() {
		case "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"gina"}`))
		case "/api/v4/personal_access_tokens/self":
			_, _ = w.Write([]byte(`{"scopes":["api","read_repository"],"expires_at":"` + time.Now().Add(3*24*time.Hour).Format("2006-01-02") + `"}`))
		case "/api/v4/projects":
			if r.URL.Query().Get("pagination") != "keyset" || r.URL.Query().Get("membership") != "true" {
				http.Error(w, "unexpected query "+r.URL.RawQuery, 400)
				return
			}
			if r.URL.Query().Get("id_after") == "" {
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/v4/projects?id_after=2&membership=true&pagination=keyset>; rel="next"`, srv.URL))
				_, _ = w.Write([]byte(`[
				 {"id":1,"path_with_namespace":"acme/api","default_branch":"main","http_url_to_repo":"https://gitlab.example/acme/api.git","ssh_url_to_repo":"git@gitlab.example:acme/api.git","visibility":"private","archived":false,"forked_from_project":null,"empty_repo":false},
				 {"id":2,"path_with_namespace":"acme/platform/infra/terraform","default_branch":"trunk","http_url_to_repo":"https://gitlab.example/acme/platform/infra/terraform.git","ssh_url_to_repo":"git@gitlab.example:acme/platform/infra/terraform.git","visibility":"internal"}]`))
				return
			}
			_, _ = w.Write([]byte(`[
			 {"id":3,"path_with_namespace":"gina/old","default_branch":"master","http_url_to_repo":"https://gitlab.example/gina/old.git","ssh_url_to_repo":"git@gitlab.example:gina/old.git","visibility":"public","archived":true,"forked_from_project":{"id":99}},
			 {"id":4,"path_with_namespace":"gina/empty","http_url_to_repo":"https://gitlab.example/gina/empty.git","ssh_url_to_repo":"git@gitlab.example:gina/empty.git","visibility":"private","empty_repo":true}]`))
		case "/api/v4/projects/acme%2Fplatform%2Finfra%2Fterraform/merge_requests":
			if r.URL.Query().Get("state") != "merged" {
				http.Error(w, "state", 400)
				return
			}
			_, _ = w.Write([]byte(`[
			 {"source_branch":"feat-a","sha":"aaa111","source_project_id":2,"target_project_id":2,"merged_at":"2026-01-02T00:00:00Z"},
			 {"source_branch":"feat-b","sha":"bbb222","source_project_id":2,"target_project_id":2,"merged_at":null},
			 {"source_branch":"feat-c","sha":"ccc333","source_project_id":77,"target_project_id":2,"merged_at":"2026-01-01T00:00:00Z"},
			 {"source_branch":"feat-a","sha":"old000","source_project_id":2,"target_project_id":2,"merged_at":"2025-12-01T00:00:00Z"},
			 {"source_branch":"other","sha":"ooo999","source_project_id":2,"target_project_id":2,"merged_at":"2025-12-01T00:00:00Z"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProv(t *testing.T, srv *httptest.Server, token string) provider.Provider {
	t.Helper()
	p, err := New(provider.Config{BaseURL: srv.URL, Token: secrets.New(token), HTTP: client(t)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContract(t *testing.T) {
	srv := fakeGitLab(t)
	nested := provider.Repo{FullName: "acme/platform/infra/terraform", Namespace: []string{"acme", "platform", "infra"}, Name: "terraform"}
	providertest.Run(t, providertest.Case{
		New:       func(t *testing.T, tok string) provider.Provider { return newProv(t, srv, tok) },
		GoodToken: good, Kind: provider.GitLab, WantLogin: "gina",
		WantRepos: []provider.Repo{
			{FullName: "acme/api", Namespace: []string{"acme"}, Name: "api", DefaultBranch: "main", CloneURL: "https://gitlab.example/acme/api.git", SSHURL: "git@gitlab.example:acme/api.git", Private: true},
			{FullName: nested.FullName, Namespace: nested.Namespace, Name: nested.Name, DefaultBranch: "trunk", CloneURL: "https://gitlab.example/acme/platform/infra/terraform.git", SSHURL: "git@gitlab.example:acme/platform/infra/terraform.git", Private: true},
			{FullName: "gina/old", Namespace: []string{"gina"}, Name: "old", DefaultBranch: "master", CloneURL: "https://gitlab.example/gina/old.git", SSHURL: "git@gitlab.example:gina/old.git", Archived: true, Fork: true},
			{FullName: "gina/empty", Namespace: []string{"gina"}, Name: "empty", CloneURL: "https://gitlab.example/gina/empty.git", SSHURL: "git@gitlab.example:gina/empty.git", Private: true, Disabled: true},
		},
		MergedRepo: nested, MergedBranches: []string{"feat-a", "feat-b", "feat-c"},
		WantMerged: map[string]string{"feat-a": "aaa111"},
	})
}

func TestNew_BaseURLNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"":                                   "https://gitlab.com/api/v4",
		"https://gitlab.example.com":         "https://gitlab.example.com/api/v4",
		"https://gitlab.example.com/":        "https://gitlab.example.com/api/v4",
		"https://gitlab.example.com/api/v4":  "https://gitlab.example.com/api/v4",
		"https://example.com/gitlab":         "https://example.com/gitlab/api/v4",
		"https://example.com/gitlab/api/v4/": "https://example.com/gitlab/api/v4",
	} {
		p, err := New(provider.Config{BaseURL: in, HTTP: client(t)})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := p.(*gl).base.String(); got != want {
			t.Errorf("%q => %q, want %q", in, got, want)
		}
	}
	if _, err := New(provider.Config{}); err == nil {
		t.Fatal("HTTP client required")
	}
	if _, err := New(provider.Config{HTTP: client(t), BaseURL: "::bad"}); err == nil {
		t.Fatal("bad URL accepted")
	}
}

func TestCheckAuth_WarnsAboutBroadScopeAndExpiry(t *testing.T) {
	a, err := newProv(t, fakeGitLab(t), good).CheckAuth(t.Context())
	if err != nil || len(a.Scopes) != 2 || a.Expires.IsZero() {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	if len(a.Warnings) != 2 || !strings.Contains(a.Warnings[0], `"api"`) || !strings.Contains(a.Warnings[1], "expires") {
		t.Fatalf("warnings=%v", a.Warnings)
	}
}

func TestCheckAuth_NoTokenMetadata_StillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/user" {
			_, _ = w.Write([]byte(`{"username":"bot"}`))
			return
		}
		http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound) // OAuth/project tokens have no PAT record
	}))
	defer srv.Close()
	a, err := newProv(t, srv, "x").CheckAuth(t.Context())
	if err != nil || a.Login != "bot" || len(a.Warnings) != 0 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
}

func TestListRepos_RefusesPaginationToForeignHost(t *testing.T) {
	var leaked bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = r.Header.Get("Authorization") != "" }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/steal>; rel="next"`, evil.URL))
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if _, err := newProv(t, srv, "x").ListRepos(t.Context()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err=%v", err)
	}
	if leaked {
		t.Fatal("token sent to a foreign host")
	}
}

func TestEndpoint_EncodesProjectPathAsOneSegment(t *testing.T) {
	p, _ := New(provider.Config{BaseURL: "https://gitlab.example.com", HTTP: client(t)})
	got := p.(*gl).endpoint("a=1", "projects", "g/sub/p", "merge_requests")
	if got != "https://gitlab.example.com/api/v4/projects/g%2Fsub%2Fp/merge_requests?a=1" {
		t.Fatalf("got %s", got)
	}
}
