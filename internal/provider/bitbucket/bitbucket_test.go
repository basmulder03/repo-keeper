// SPDX-License-Identifier: Apache-2.0

package bitbucket

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

const (
	good = "ATATT-GOODtokenValue0123456789"
	full = "aaa1112223334445556667778889990001112222" // 40 hex
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

func fixtureRepo(uuid, full, branch string, private bool, extra string) string {
	mb := `null`
	if branch != "" {
		mb = `{"name":"` + branch + `"}`
	}
	ws := strings.SplitN(full, "/", 2)
	return fmt.Sprintf(`{"uuid":"%s","full_name":"%s","scm":"git","is_private":%t,"mainbranch":%s,%s"links":{"clone":[
	 {"name":"https","href":"https://someone@bitbucket.example/%s.git"},{"name":"ssh","href":"git@bitbucket.example:%s.git"}]},"workspace":"%s"}`,
		uuid, full, private, mb, extra, full, full, ws[0])
}

// fakeBitbucket serves two workspaces (the first with two repository pages), Mercurial and malformed entries, and merged PRs.
func fakeBitbucket(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+good || r.UserAgent() != "repo-keeper/test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"message":"Token is invalid or expired"}}`))
			return
		}
		q := r.URL.Query()
		switch r.URL.EscapedPath() {
		case "/2.0/user":
			w.Header().Set("X-OAuth-Scopes", "read:repository:bitbucket, read:pullrequest:bitbucket")
			_, _ = w.Write([]byte(`{"nickname":"gina","display_name":"Gina G"}`))
		case "/2.0/user/workspaces":
			_, _ = w.Write([]byte(`{"values":[{"workspace":{"slug":"acme"}},{"workspace":{"slug":"gina"}}]}`))
		case "/2.0/repositories/acme":
			if q.Get("role") != "member" {
				http.Error(w, "role", 400)
				return
			}
			if q.Get("page") == "" {
				_, _ = fmt.Fprintf(w, `{"next":"%s/2.0/repositories/acme?role=member&page=2","values":[%s,%s,{"uuid":"{hg}","full_name":"acme/legacy","scm":"hg"}]}`,
					srv.URL, fixtureRepo("{1}", "acme/api", "main", true, ""), fixtureRepo("{2}", "acme/web", "trunk", false, ""))
				return
			}
			_, _ = fmt.Fprintf(w, `{"values":[%s,{"uuid":"{bad}","full_name":"not-a-slash-name"}]}`, fixtureRepo("{3}", "acme/fork", "main", true, `"parent":{"full_name":"up/stream"},`))
		case "/2.0/repositories/gina":
			// "gina/shared" is also visible through acme in real life; the same uuid must not be listed twice
			_, _ = fmt.Fprintf(w, `{"values":[%s,%s,%s]}`, fixtureRepo("{4}", "gina/empty", "", true, ""), fixtureRepo("{2}", "acme/web", "trunk", false, ""), fixtureRepo("{5}", "gina/notes", "master", true, ""))
		case "/2.0/repositories/acme/web/pullrequests":
			if q.Get("state") != "MERGED" || !strings.Contains(q.Get("fields"), "values.source.commit.hash") {
				http.Error(w, "query "+r.URL.RawQuery, 400)
				return
			}
			_, _ = fmt.Fprintf(w, `{"values":[
			 {"source":{"branch":{"name":"feat-a"},"commit":{"hash":"aaa111222333"},"repository":{"full_name":"acme/web"}},"destination":{"repository":{"full_name":"acme/web"}}},
			 {"source":{"branch":{"name":"feat-c"},"commit":{"hash":"ccc333444555"},"repository":{"full_name":"someone/web"}},"destination":{"repository":{"full_name":"acme/web"}}},
			 {"source":{"branch":{"name":"feat-d"},"commit":{"hash":"ddd444555666"},"repository":null},"destination":{"repository":{"full_name":"acme/web"}}},
			 {"source":{"branch":{"name":"feat-e"},"commit":{"hash":"eee555666777"},"repository":{"full_name":"acme/web"}},"destination":{"repository":{"full_name":"acme/web"}}},
			 {"source":{"branch":{"name":"feat-a"},"commit":{"hash":"old000111222"},"repository":{"full_name":"acme/web"}},"destination":{"repository":{"full_name":"acme/web"}}},
			 {"source":{"branch":{"name":"other"},"commit":{"hash":"ooo999000111"},"repository":{"full_name":"acme/web"}},"destination":{"repository":{"full_name":"acme/web"}}}]}`)
		case "/2.0/repositories/acme/web/commit/aaa111222333":
			_, _ = fmt.Fprintf(w, `{"hash":"%s"}`, full)
		case "/2.0/repositories/acme/web/commit/eee555666777":
			http.Error(w, `{"error":{"message":"ambiguous prefix"}}`, http.StatusBadRequest)
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
	srv := fakeBitbucket(t)
	web := provider.Repo{FullName: "acme/web", Namespace: []string{"acme"}, Name: "web"}
	want := func(full, branch string, private, fork, disabled bool) provider.Repo {
		ns := strings.SplitN(full, "/", 2)
		return provider.Repo{FullName: full, Namespace: ns[:1], Name: ns[1], DefaultBranch: branch, Private: private, Fork: fork, Disabled: disabled,
			CloneURL: "https://bitbucket.example/" + full + ".git", SSHURL: "git@bitbucket.example:" + full + ".git"}
	}
	providertest.Run(t, providertest.Case{
		New:       func(t *testing.T, tok string) provider.Provider { return newProv(t, srv, tok) },
		GoodToken: good, Kind: provider.Bitbucket, WantLogin: "gina",
		WantRepos: []provider.Repo{
			want("acme/api", "main", true, false, false),
			want("acme/web", "trunk", false, false, false),
			want("acme/fork", "main", true, true, false),
			want("gina/empty", "", true, false, true),
			want("gina/notes", "master", true, false, false),
		},
		MergedRepo: web, MergedBranches: []string{"feat-a", "feat-b", "feat-c", "feat-d"},
		WantMerged: map[string]string{"feat-a": full},
	})
}

func TestMergedBranches_ShortHashIsExpanded_AmbiguousIsSkipped(t *testing.T) {
	p := newProv(t, fakeBitbucket(t), good)
	web := provider.Repo{FullName: "acme/web", Namespace: []string{"acme"}, Name: "web"}
	got, err := p.MergedBranches(t.Context(), web, []string{"feat-a", "feat-e"})
	if err != nil {
		t.Fatal(err)
	}
	if got["feat-a"] != full || len(got) != 1 {
		t.Fatalf("a full id is required for the exact comparison in cleanup, and an unresolvable one must be left out: %v", got)
	}
}

func TestCheckAuth_ScopesFromHeader_WriteScopeWarns(t *testing.T) {
	a, err := newProv(t, fakeBitbucket(t), good).CheckAuth(t.Context())
	if err != nil || len(a.Scopes) != 2 || len(a.Warnings) != 0 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	for scope, warns := range map[string]bool{"write:repository:bitbucket": true, "admin:pullrequest:bitbucket": true, "repository:write": true, "read:user:bitbucket": false} {
		if got := len(warnings(provider.Auth{Scopes: []string{scope}})) > 0; got != warns {
			t.Errorf("%s: warns=%v want %v", scope, got, warns)
		}
	}
	if w := warnings(provider.Auth{}); len(w) != 1 || !strings.Contains(w[0], "did not report") {
		t.Fatalf("unknown scopes must be flagged: %v", w)
	}
}

func TestCheckAuth_NoUser_IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	if _, err := newProv(t, srv, "x").CheckAuth(t.Context()); err == nil {
		t.Fatal("an answer without a user is not a verified credential")
	}
}

func TestListRepos_CloneURLNeverCarriesAUsername(t *testing.T) {
	got, err := newProv(t, fakeBitbucket(t), good).ListRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if strings.Contains(r.CloneURL, "@") {
			t.Errorf("%s: %s", r.FullName, r.CloneURL)
		}
	}
}

func TestNew_BaseURLNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "https://api.bitbucket.org/2.0",
		"https://api.bitbucket.org":     "https://api.bitbucket.org/2.0",
		"https://api.bitbucket.org/":    "https://api.bitbucket.org/2.0",
		"https://proxy.example/bb/2.0/": "https://proxy.example/bb/2.0",
	} {
		p, err := New(provider.Config{BaseURL: in, HTTP: client(t)})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := p.(*bb).base.String(); got != want {
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

func TestListRepos_RefusesPaginationToForeignHost(t *testing.T) {
	var leaked bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = r.Header.Get("Authorization") != "" }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"next":"%s/steal","values":[]}`, evil.URL)
	}))
	defer srv.Close()
	if _, err := newProv(t, srv, "x").ListRepos(t.Context()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err=%v", err)
	}
	if leaked {
		t.Fatal("token sent to a foreign host")
	}
}

func TestMergedBranches_RejectsUnexpectedNamespace(t *testing.T) {
	p := newProv(t, fakeBitbucket(t), good)
	if _, err := p.MergedBranches(t.Context(), provider.Repo{FullName: "a/b/c", Namespace: []string{"a", "b"}, Name: "c"}, []string{"x"}); err == nil {
		t.Fatal("a nested namespace must be refused, not mis-addressed")
	}
}

func TestExpand_RejectsGarbageHashesWithoutARequest(t *testing.T) {
	p := newProv(t, httptest.NewServer(http.NotFoundHandler()), good).(*bb)
	for _, h := range []string{"", "xyz", "../../etc", "abc", strings.Repeat("g", 12)} {
		if _, err := p.expand(t.Context(), "acme", "web", h); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
	if got, err := p.expand(t.Context(), "acme", "web", strings.ToUpper(full)); err != nil || got != full {
		t.Fatalf("a full id needs no request: %q %v", got, err)
	}
}
