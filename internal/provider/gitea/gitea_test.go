// SPDX-License-Identifier: Apache-2.0

package gitea

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/provider/providertest"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

const good = "gitea-GOODtokenValue0123456789"

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

// fakeGitea serves a fixture over two Link-paginated pages.
func fakeGitea(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+good || r.UserAgent() != "repo-keeper/test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"token is required"}`))
			return
		}
		switch r.URL.EscapedPath() {
		case "/api/v1/user":
			_, _ = w.Write([]byte(`{"login":"gina"}`))
		case "/api/v1/user/repos":
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/user/repos?limit=50&page=2>; rel="next"`, srv.URL))
				_, _ = w.Write([]byte(`[
				 {"id":1,"name":"api","full_name":"acme/api","owner":{"login":"acme"},"default_branch":"main","clone_url":"https://git.example/acme/api.git","ssh_url":"git@git.example:acme/api.git","private":true},
				 {"id":2,"name":"web","full_name":"acme/web","owner":{"login":"acme"},"default_branch":"trunk","clone_url":"https://git.example/acme/web.git","ssh_url":"git@git.example:acme/web.git"}]`))
				return
			}
			_, _ = w.Write([]byte(`[
			 {"id":3,"name":"old","full_name":"gina/old","owner":{"login":"gina"},"default_branch":"master","clone_url":"https://git.example/gina/old.git","ssh_url":"git@git.example:gina/old.git","archived":true,"fork":true},
			 {"id":4,"name":"empty","full_name":"gina/empty","owner":{"login":"gina"},"clone_url":"https://git.example/gina/empty.git","ssh_url":"git@git.example:gina/empty.git","private":true,"empty":true}]`))
		case "/api/v1/repos/acme/web/pulls":
			if r.URL.Query().Get("state") != "closed" {
				http.Error(w, "state", 400)
				return
			}
			_, _ = w.Write([]byte(`[
			 {"merged":true,"head":{"ref":"feat-a","sha":"aaa111","repo":{"id":2}},"base":{"repo":{"id":2}}},
			 {"merged":false,"head":{"ref":"feat-b","sha":"bbb222","repo":{"id":2}},"base":{"repo":{"id":2}}},
			 {"merged":true,"head":{"ref":"feat-c","sha":"ccc333","repo":{"id":77}},"base":{"repo":{"id":2}}},
			 {"merged":true,"head":{"ref":"feat-d","sha":"ddd444","repo":null},"base":{"repo":{"id":2}}},
			 {"merged":true,"head":{"ref":"feat-a","sha":"old000","repo":{"id":2}},"base":{"repo":{"id":2}}},
			 {"merged":true,"head":{"ref":"other","sha":"ooo999","repo":{"id":2}},"base":{"repo":{"id":2}}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProv(t *testing.T, srv *httptest.Server, kind provider.Kind, token string) provider.Provider {
	t.Helper()
	p, err := New(kind, provider.Config{BaseURL: srv.URL, Token: secrets.New(token), HTTP: client(t)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContract(t *testing.T) {
	for _, kind := range []provider.Kind{provider.Gitea, provider.Forgejo} {
		t.Run(string(kind), func(t *testing.T) {
			srv := fakeGitea(t)
			web := provider.Repo{FullName: "acme/web", Namespace: []string{"acme"}, Name: "web"}
			providertest.Run(t, providertest.Case{
				New:       func(t *testing.T, tok string) provider.Provider { return newProv(t, srv, kind, tok) },
				GoodToken: good, Kind: kind, WantLogin: "gina",
				WantRepos: []provider.Repo{
					{FullName: "acme/api", Namespace: []string{"acme"}, Name: "api", DefaultBranch: "main", CloneURL: "https://git.example/acme/api.git", SSHURL: "git@git.example:acme/api.git", Private: true},
					{FullName: web.FullName, Namespace: web.Namespace, Name: web.Name, DefaultBranch: "trunk", CloneURL: "https://git.example/acme/web.git", SSHURL: "git@git.example:acme/web.git"},
					{FullName: "gina/old", Namespace: []string{"gina"}, Name: "old", DefaultBranch: "master", CloneURL: "https://git.example/gina/old.git", SSHURL: "git@git.example:gina/old.git", Archived: true, Fork: true},
					{FullName: "gina/empty", Namespace: []string{"gina"}, Name: "empty", CloneURL: "https://git.example/gina/empty.git", SSHURL: "git@git.example:gina/empty.git", Private: true, Disabled: true},
				},
				MergedRepo: web, MergedBranches: []string{"feat-a", "feat-b", "feat-c", "feat-d"},
				WantMerged: map[string]string{"feat-a": "aaa111"},
			})
		})
	}
}

func TestNew_BaseURLRequiredAndNormalised(t *testing.T) {
	for in, want := range map[string]string{
		"https://git.example.com":           "https://git.example.com/api/v1",
		"https://git.example.com/":          "https://git.example.com/api/v1",
		"https://git.example.com/api/v1":    "https://git.example.com/api/v1",
		"https://example.com/gitea/api/v1/": "https://example.com/gitea/api/v1",
		"https://example.com/gitea":         "https://example.com/gitea/api/v1",
	} {
		p, err := New(provider.Gitea, provider.Config{BaseURL: in, HTTP: client(t)})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := p.(*gt).base.String(); got != want {
			t.Errorf("%q => %q, want %q", in, got, want)
		}
	}
	if _, err := New(provider.Gitea, provider.Config{HTTP: client(t)}); err == nil || !strings.Contains(err.Error(), "base_url is required") {
		t.Fatalf("empty base_url: %v", err)
	}
	if _, err := New(provider.Gitea, provider.Config{BaseURL: "https://x"}); err == nil {
		t.Fatal("HTTP client required")
	}
	if _, err := New(provider.Gitea, provider.Config{HTTP: client(t), BaseURL: "::bad"}); err == nil {
		t.Fatal("bad URL accepted")
	}
}

func TestRegistered_BothKinds(t *testing.T) {
	for _, k := range []provider.Kind{provider.Gitea, provider.Forgejo} {
		if !provider.Known(k) {
			t.Errorf("%s not registered", k)
		}
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
	if _, err := newProv(t, srv, provider.Gitea, "x").ListRepos(t.Context()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err=%v", err)
	}
	if leaked {
		t.Fatal("token sent to a foreign host")
	}
}

func TestMergedBranches_RejectsUnexpectedNamespace(t *testing.T) {
	p := newProv(t, fakeGitea(t), provider.Gitea, good)
	if _, err := p.MergedBranches(t.Context(), provider.Repo{FullName: "a/b/c", Namespace: []string{"a", "b"}, Name: "c"}, []string{"x"}); err == nil {
		t.Fatal("nested namespace must be refused, not mis-addressed")
	}
}

func TestCheckAuth_NoLogin_IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	if _, err := newProv(t, srv, provider.Gitea, "x").CheckAuth(t.Context()); err == nil {
		t.Fatal("an answer without a login is not a verified credential")
	}
}

func TestEndpoint_EscapesEachSegment(t *testing.T) {
	p, _ := New(provider.Gitea, provider.Config{BaseURL: "https://git.example.com", HTTP: client(t)})
	got := p.(*gt).endpoint("a=1", "repos", "o w", "r/x", "pulls")
	if got != "https://git.example.com/api/v1/repos/o%20w/r%2Fx/pulls?a=1" {
		t.Fatalf("got %s", got)
	}
}
