// SPDX-License-Identifier: Apache-2.0

package gitx_test

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

const secret = "s3cr3t-token-value"

// httpRemote serves e.Origin over smart HTTP behind basic auth (user "x-access-token", password secret).
func httpRemote(t *testing.T, e *gitxtest.Env) (srv *httptest.Server, correctAuths *atomic.Int32) {
	t.Helper()
	execPath := e.Git(e.Root, "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not available")
	}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + e.Root, "GIT_HTTP_EXPORT_ALL=1"}, InheritEnv: []string{"PATH"}}
	correctAuths = new(atomic.Int32)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "x-access-token" || p != secret {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		correctAuths.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, correctAuths
}

func helper(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func cred(t *testing.T, srv *httptest.Server, tok string) *gitx.Cred {
	return &gitx.Cred{Host: strings.TrimPrefix(srv.URL, "http://"), Username: "x-access-token", Secret: secrets.New(tok), Helper: helper(t)}
}

func TestCred_AuthenticatesLsRemoteAndClone_WithoutLeaking(t *testing.T) {
	e := gitxtest.New(t)
	srv, auths := httpRemote(t, e)
	url := srv.URL + "/origin.git"
	scratch := e.Repo(e.Work).WithCred(cred(t, srv, secret))
	e.Git(e.Work, "remote", "set-url", "origin", url)

	st, err := scratch.LsRemote(t.Context(), "origin")
	if err != nil || st.DefaultBranch != "main" || auths.Load() == 0 {
		t.Fatalf("st=%+v err=%v auths=%d", st, err, auths.Load())
	}

	dest := filepath.Join(e.Root, "cloned", "repo")
	if err := e.R.Clone(t.Context(), url, dest, cred(t, srv, secret), false); err != nil {
		t.Fatal(err)
	}
	if got := e.Git(dest, "remote", "get-url", "origin"); got != url || strings.Contains(got, secret) {
		t.Fatalf("remote url=%q", got)
	}
	cfg, _ := os.ReadFile(filepath.Join(dest, ".git", "config"))
	if strings.Contains(string(cfg), secret) {
		t.Fatal("secret persisted in .git/config")
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("working tree missing: %v", err)
	}
}

func TestCred_Rejections(t *testing.T) {
	e := gitxtest.New(t)
	srv, _ := httpRemote(t, e)
	e.Git(e.Work, "remote", "set-url", "origin", srv.URL+"/origin.git")
	g := e.Repo(e.Work)

	for name, c := range map[string]*gitx.Cred{
		"no credential":     nil,
		"wrong secret":      cred(t, srv, "wrong"),
		"wrong host scoped": {Host: "evil.example", Username: "x-access-token", Secret: secrets.New(secret), Helper: helper(t)},
	} {
		_, err := g.WithCred(c).LsRemote(t.Context(), "origin")
		if err == nil {
			t.Errorf("%s: must fail", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the secret: %v", name, err)
		}
	}
}

func TestClone_FailureLeavesNoDirectory(t *testing.T) {
	e := gitxtest.New(t)
	dest := filepath.Join(e.Root, "x", "y")
	if err := e.R.Clone(t.Context(), filepath.Join(e.Root, "missing.git"), dest, nil, false); err == nil {
		t.Fatal("clone of missing repo must fail")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("half-cloned directory left behind")
	}
	if err := e.R.Clone(t.Context(), e.Origin, e.Work, nil, false); err == nil {
		t.Fatal("must refuse an existing destination")
	}
	if _, err := os.Stat(filepath.Join(e.Work, "README.md")); err != nil {
		t.Fatal("existing destination was damaged")
	}
}

func TestCred_CAFileReachesGitAsEnvOnly(t *testing.T) {
	var seen []string
	r := gitx.Runner{Bin: "git", Observe: func(rec gitx.CommandRecord) { seen = append(seen, rec.Args...) }}
	_ = r
	c := &gitx.Cred{Host: "h", Username: "u", Secret: secrets.New("tok"), Helper: "/bin/true", CAFile: "/etc/private-ca.pem"}
	e := gitxtest.New(t)
	e.R.Observe = func(rec gitx.CommandRecord) { seen = append(seen, rec.Args...) }
	_, _ = e.Repo(e.Work).WithCred(c).LsRemote(t.Context(), "origin")
	for _, a := range seen {
		if strings.Contains(a, "private-ca") || strings.Contains(a, "tok") {
			t.Fatalf("credential material in argv: %q", a)
		}
	}
}

func TestCred_CloneAndLsRemote_InPathsWithSpaces(t *testing.T) {
	e := gitxtest.New(t)
	srv, auths := httpRemote(t, e)
	url := srv.URL + "/origin.git"
	dest := filepath.Join(e.Root, "azuredevops", "My Project", "my repo")
	if err := e.R.Clone(t.Context(), url, dest, cred(t, srv, secret), false); err != nil {
		t.Fatalf("Azure DevOps style names contain spaces: %v", err)
	}
	st, err := e.R.Repo(dest).WithCred(cred(t, srv, secret)).LsRemote(t.Context(), "origin")
	if err != nil || st.DefaultBranch != "main" || auths.Load() == 0 {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}
