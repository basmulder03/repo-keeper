// SPDX-License-Identifier: Apache-2.0

package azuredevops

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	good  = "azpat-GOODtokenValue0123456789abcdef"
	sha40 = "0123456789abcdef0123456789abcdef01234567"
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

func fixtureRepo(id, name, branch string, extra string) string {
	db := ""
	if branch != "" {
		db = `"defaultBranch":"refs/heads/` + branch + `",`
	}
	return fmt.Sprintf(`{"id":"%s","name":"%s",%s%s"remoteUrl":"https://acme@dev.azure.com/acme/p/_git/%s","sshUrl":"git@ssh.dev.azure.com:v3/acme/p/%s"}`,
		id, name, db, extra, strings.ReplaceAll(name, " ", "%20"), strings.ReplaceAll(name, " ", "%20"))
}

func pr(status, ref, sha, extra string) string {
	return fmt.Sprintf(`{"status":"%s","sourceRefName":"%s","lastMergeSourceCommit":{"commitId":"%s"},%s"pullRequestId":1}`, status, ref, sha, extra)
}

// fakeADO serves organization "acme": projects over two continuation pages (one with a space in its name, one public).
func fakeADO(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "" || pass != good || r.UserAgent() != "repo-keeper/test" {
			// what Azure DevOps really does for a bad PAT: 203 and the HTML sign-in page
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNonAuthoritativeInfo)
			_, _ = w.Write([]byte(`<html>Sign in to your account</html>`))
			return
		}
		if r.URL.Query().Get("api-version") != "7.1" {
			http.Error(w, "api-version", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/acme/_apis/projects":
			if r.URL.Query().Get("continuationToken") == "" {
				w.Header().Set("x-ms-continuationtoken", "2")
				_, _ = w.Write([]byte(`{"value":[{"name":"Web Team","visibility":"private"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"value":[{"name":"Public Docs","visibility":"public"}]}`))
		case "/acme/Web%20Team/_apis/git/repositories":
			_, _ = fmt.Fprintf(w, `{"count":5,"value":[%s,%s,%s,%s,%s]}`,
				fixtureRepo("1", "api", "main", `"project":{"name":"Web Team"},`),
				fixtureRepo("2", "my repo", "trunk", ""),
				fixtureRepo("3", "forked", "main", `"isFork":true,`),
				fixtureRepo("4", "empty", "", ""),
				fixtureRepo("5", "off", "main", `"isDisabled":true,`))
		case "/acme/Public%20Docs/_apis/git/repositories":
			_, _ = fmt.Fprintf(w, `{"value":[%s,%s]}`, fixtureRepo("6", "site", "main", ""), fixtureRepo("1", "api", "main", "")) // id 1 repeats: listed once
		case "/acme/Web%20Team/_apis/git/repositories/my%20repo/pullrequests":
			if r.URL.Query().Get("searchCriteria.status") != "completed" {
				http.Error(w, "status", 400)
				return
			}
			_, _ = fmt.Fprintf(w, `{"count":6,"value":[%s,%s,%s,%s,%s,%s]}`,
				pr("completed", "refs/heads/feat-a", sha40, ""),
				pr("abandoned", "refs/heads/feat-b", sha40, ""),
				pr("completed", "refs/heads/feat-c", sha40, `"forkSource":{"repository":{"id":"x"}},`),
				pr("completed", "refs/heads/feat-d", "abc123", ""),
				pr("completed", "refs/tags/feat-e", sha40, ""),
				pr("completed", "refs/heads/other", sha40, ""))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProv(t *testing.T, srv *httptest.Server, token string) provider.Provider {
	t.Helper()
	p, err := New(provider.Config{BaseURL: srv.URL + "/acme", Token: secrets.New(token), HTTP: client(t)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContract(t *testing.T) {
	srv := fakeADO(t)
	mine := provider.Repo{FullName: "Web Team/my repo", Namespace: []string{"Web Team"}, Name: "my repo"}
	want := func(proj, name, branch string, private, fork, disabled bool) provider.Repo {
		esc := strings.ReplaceAll(name, " ", "%20")
		return provider.Repo{FullName: proj + "/" + name, Namespace: []string{proj}, Name: name, DefaultBranch: branch, Private: private, Fork: fork, Disabled: disabled,
			CloneURL: "https://dev.azure.com/acme/p/_git/" + esc, SSHURL: "git@ssh.dev.azure.com:v3/acme/p/" + esc}
	}
	providertest.Run(t, providertest.Case{
		New:       func(t *testing.T, tok string) provider.Provider { return newProv(t, srv, tok) },
		GoodToken: good, Kind: provider.AzureDevOps, WantLogin: "acme",
		WantRepos: []provider.Repo{
			want("Web Team", "api", "main", true, false, false),
			want("Web Team", "my repo", "trunk", true, false, false),
			want("Web Team", "forked", "main", true, true, false),
			want("Web Team", "empty", "", true, false, true),
			want("Web Team", "off", "main", true, false, true),
			want("Public Docs", "site", "main", false, false, false),
		},
		MergedRepo: mine, MergedBranches: []string{"feat-a", "feat-b", "feat-c", "feat-d", "feat-e"},
		WantMerged: map[string]string{"feat-a": sha40},
	})
}

func TestMergedBranches_PagesWithSkip(t *testing.T) {
	var skips []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		skips = append(skips, r.URL.Query().Get("$skip"))
		var items []string
		if skip == 0 {
			for i := 0; i < pageSize; i++ {
				items = append(items, pr("completed", fmt.Sprintf("refs/heads/old-%d", i), sha40, ""))
			}
		} else {
			items = append(items, pr("completed", "refs/heads/target", sha40, ""))
		}
		_, _ = fmt.Fprintf(w, `{"value":[%s]}`, strings.Join(items, ","))
	}))
	defer srv.Close()
	p, _ := New(provider.Config{BaseURL: srv.URL + "/acme", Token: secrets.New("x"), HTTP: client(t)})
	got, err := p.MergedBranches(t.Context(), provider.Repo{FullName: "p/r"}, []string{"target"})
	if err != nil || got["target"] != sha40 || strings.Join(skips, ",") != "0,100" {
		t.Fatalf("got=%v err=%v skips=%v", got, err, skips)
	}
}

func TestCheckAuth_ReportsOrganisationAndWarnsAboutWhatItCannotKnow(t *testing.T) {
	a, err := newProv(t, fakeADO(t), good).CheckAuth(t.Context())
	if err != nil || a.Login != "acme" || len(a.Warnings) != 1 || !strings.Contains(a.Warnings[0], "does not report") {
		t.Fatalf("a=%+v err=%v", a, err)
	}
}

func TestGet_SendsPATAsBasicWithEmptyUser(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	defer srv.Close()
	if _, err := newProv(t, srv, "secret").CheckAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":secret")); got != want {
		t.Fatalf("Authorization=%q", got)
	}
}

func TestNew_BaseURLRequiredWithOrganisation(t *testing.T) {
	for in, org := range map[string]string{
		"https://dev.azure.com/acme":              "acme",
		"https://dev.azure.com/acme/":             "acme",
		"https://tfs.example.com/tfs/Collection1": "Collection1",
	} {
		p, err := New(provider.Config{BaseURL: in, HTTP: client(t)})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if p.(*ado).org != org {
			t.Errorf("%q => org %q", in, p.(*ado).org)
		}
	}
	for _, bad := range []string{"", "https://dev.azure.com", "https://dev.azure.com/", "::bad", "/acme"} {
		if _, err := New(provider.Config{BaseURL: bad, HTTP: client(t)}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := New(provider.Config{BaseURL: "https://dev.azure.com/acme"}); err == nil {
		t.Fatal("HTTP client required")
	}
}

func TestMergedBranches_RejectsUnexpectedNames(t *testing.T) {
	p := newProv(t, fakeADO(t), good)
	for _, bad := range []string{"", "onlyone", "a/b/c", "/b", "a/"} {
		if _, err := p.MergedBranches(t.Context(), provider.Repo{FullName: bad}, []string{"x"}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGet_ForbiddenWithoutRedirectIsAPIErrorNotAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"no access"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := newProv(t, srv, "x").ListRepos(t.Context())
	var ae *APIError
	if err == nil || !strings.Contains(err.Error(), "no access") || !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("err=%v", err)
	}
}
