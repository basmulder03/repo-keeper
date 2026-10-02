// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

const ghToken = "ghp_CANARYtokenValue0123456789abcdef"

type fakeGH struct {
	mu     sync.Mutex
	repos  []map[string]any
	status int // when non-zero every request answers with it
	prs    string
	hits   int
}

func (f *fakeGH) set(repos ...map[string]any) {
	f.mu.Lock()
	f.repos = repos
	f.mu.Unlock()
}

func (f *fakeGH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+ghToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	switch {
	case r.URL.Path == "/user":
		_, _ = w.Write([]byte(`{"login":"octo"}`))
	case r.URL.Path == "/user/repos":
		_ = json.NewEncoder(w).Encode(f.repos)
	case strings.HasSuffix(r.URL.Path, "/pulls"):
		_, _ = w.Write([]byte(f.prs))
	default:
		http.NotFound(w, r)
	}
}

func ghRepo(full, cloneURL string, archived bool) map[string]any {
	return map[string]any{"id": len(full), "name": filepath.Base(full), "full_name": full, "clone_url": cloneURL, "ssh_url": cloneURL, "default_branch": "main", "archived": archived}
}

func tokenFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "gh-token")
	if err := os.WriteFile(p, []byte(ghToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func acctCfg(root, base, tokFile, extra string) string {
	return fmt.Sprintf(`[general]
root = %q
interval = "30m"
[cleanup]
mode = "off"
min_age = "0s"
[[account]]
name = "gh"
provider = "github"
base_url = %q
token_file = %q
%s
`, root, base, tokFile, extra)
}

func (r *rig) account(t *testing.T) (a struct {
	Status, Login, Err string
	Repos              int
}) {
	t.Helper()
	as, err := r.d.Store.ListAccounts(context.Background())
	if err != nil || len(as) == 0 {
		return a
	}
	return struct {
		Status, Login, Err string
		Repos              int
	}{as[0].Status, as[0].Login, as[0].Error, as[0].RepoCount}
}

func (r *rig) hasEvent(code string) bool {
	ev, _ := r.d.Store.RecentEvents(context.Background(), 50)
	for _, e := range ev {
		if e.Code == code {
			return true
		}
	}
	return false
}

func TestDaemon_Discovery_FiltersClonesAndRecordsAccount(t *testing.T) {
	e := gitxtest.New(t)
	gh := &fakeGH{}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	gh.set(
		ghRepo("acme/api", e.Origin, false),
		ghRepo("acme/old", e.Origin, true), // archived: skipped
		ghRepo("other/x", e.Origin, false), // not included
	)
	root := filepath.Join(t.TempDir(), "code")
	r := start(t, e, acctCfg(root, srv.URL, tokenFile(t), `include = ["acme/*"]`))

	dest := filepath.Join(root, "github", "acme", "api")
	r.waitFor(t, "clone", func() bool {
		rs := r.repos(t)
		return len(rs) == 1 && rs[0].LastReason == "cloned"
	})
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("working tree missing: %v", err)
	}
	rs := r.repos(t)
	if rs[0].Source != "gh" || rs[0].FullName != "acme/api" || rs[0].DefaultBranch != "main" {
		t.Fatalf("repo=%+v", rs[0])
	}
	if a := r.account(t); a.Status != "ok" || a.Login != "octo" || a.Repos != 1 {
		t.Fatalf("account=%+v", a)
	}
	if !r.hasEvent("cloned") {
		t.Fatal("clone event missing")
	}
	// a second pass after the interval syncs the existing clone instead of cloning again
	other := e.Clone("t1")
	want := e.Commit(other, "n.txt", "n", "n")
	e.Git(other, "push", "-q", "origin", "main")
	r.clk.BlockUntil(2, time.Second)
	r.clk.Advance(45 * time.Minute)
	r.waitFor(t, "fast-forward of the clone", func() bool {
		out, err := e.R.Run(context.Background(), dest, "rev-parse", "main")
		return err == nil && strings.TrimSpace(out) == want
	})
}

func TestDaemon_Discovery_MissingCredential(t *testing.T) {
	e := gitxtest.New(t)
	srv := httptest.NewServer(&fakeGH{})
	defer srv.Close()
	r := start(t, e, acctCfg(t.TempDir(), srv.URL, filepath.Join(t.TempDir(), "nope"), ""))
	r.waitFor(t, "no-credential status", func() bool { return r.account(t).Status == "no-credential" })
	if len(r.repos(t)) != 0 || !r.hasEvent("account-no-credential") {
		t.Fatal("must report and track nothing")
	}
}

func TestDaemon_Discovery_AuthFailure_FlaggedAndRetriedSlowly(t *testing.T) {
	e := gitxtest.New(t)
	gh := &fakeGH{status: http.StatusUnauthorized}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	r := start(t, e, acctCfg(t.TempDir(), srv.URL, tokenFile(t), ""))
	r.waitFor(t, "auth-failed", func() bool { return r.account(t).Status == "auth-failed" })
	if !r.hasEvent("account-auth-failed") {
		t.Fatal("event missing")
	}
	gh.mu.Lock()
	before := gh.hits
	gh.mu.Unlock()
	r.clk.BlockUntil(3, time.Second)
	r.clk.Advance(10 * time.Minute) // well inside the retry window: no new API calls
	time.Sleep(100 * time.Millisecond)
	gh.mu.Lock()
	after := gh.hits
	gh.mu.Unlock()
	if after != before {
		t.Fatalf("hammered the API after an auth failure: %d -> %d", before, after)
	}
}

func TestDaemon_Discovery_RepoDisappears_MarkedMissingLocalCloneKept(t *testing.T) {
	e := gitxtest.New(t)
	gh := &fakeGH{}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	gh.set(ghRepo("acme/api", e.Origin, false), ghRepo("acme/gone", e.Origin, false))
	root := filepath.Join(t.TempDir(), "code")
	r := start(t, e, acctCfg(root, srv.URL, tokenFile(t), `discovery_interval = "1h"`))
	r.waitFor(t, "discovery", func() bool { return len(r.repos(t)) == 2 })
	r.clk.BlockUntil(3, time.Second)
	r.clk.Advance(10 * time.Second) // the second repo is staggered 2s after the first
	r.waitFor(t, "both cloned", func() bool {
		n := 0
		for _, x := range r.repos(t) {
			if x.LastReason == "cloned" {
				n++
			}
		}
		return n == 2
	})

	gh.set(ghRepo("acme/api", e.Origin, false))
	r.clk.BlockUntil(3, time.Second)
	r.clk.Advance(7 * time.Hour)
	r.waitFor(t, "gone repo deactivated", func() bool { return len(r.repos(t)) == 1 })
	if _, err := os.Stat(filepath.Join(root, "github", "acme", "gone", "README.md")); err != nil {
		t.Fatal("local clone of a repo that vanished upstream must never be deleted")
	}
	if !r.hasEvent("discovery") {
		t.Fatal("discovery summary event missing")
	}
}

func TestDaemon_PathOccupied_NeverOverwritten(t *testing.T) {
	e := gitxtest.New(t)
	gh := &fakeGH{}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	gh.set(ghRepo("acme/api", e.Origin, false))
	root := filepath.Join(t.TempDir(), "code")
	dest := filepath.Join(root, "github", "acme", "api")
	_ = os.MkdirAll(dest, 0o750)
	_ = os.WriteFile(filepath.Join(dest, "precious.txt"), []byte("mine"), 0o600)
	r := start(t, e, acctCfg(root, srv.URL, tokenFile(t), ""))
	r.waitFor(t, "path-occupied attention", func() bool {
		rs := r.repos(t)
		return len(rs) == 1 && rs[0].NeedsAttention && rs[0].LastReason == "path-occupied"
	})
	if b, _ := os.ReadFile(filepath.Join(dest, "precious.txt")); string(b) != "mine" {
		t.Fatal("existing directory damaged")
	}
}

func TestDaemon_SquashMergedBranch_CleanedViaProviderPR(t *testing.T) {
	e := gitxtest.New(t)
	// origin has a two-commit branch "feat" (patch-id matching cannot prove a squash of two commits)
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f1.txt", "1", "one")
	e.Commit(e.Work, "f2.txt", "2", "two")
	e.Git(e.Work, "push", "-q", "-u", "origin", "feat")
	e.Git(e.Work, "checkout", "-q", "main")

	gh := &fakeGH{}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	gh.set(ghRepo("acme/api", e.Origin, false))
	root := filepath.Join(t.TempDir(), "code")
	r := start(t, e, acctCfg(root, srv.URL, tokenFile(t), `cleanup = "auto"`+"\n"))
	dest := filepath.Join(root, "github", "acme", "api")
	r.waitFor(t, "clone", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].LastReason == "cloned" })

	// the developer works on feat locally; upstream squash-merges and deletes the branch
	e.Git(dest, "branch", "--track", "feat", "origin/feat")
	tip := e.Git(dest, "rev-parse", "feat")
	e.Git(e.Work, "merge", "-q", "--squash", "feat")
	e.Git(e.Work, "commit", "-q", "-m", "squash feat")
	e.Git(e.Work, "push", "-q", "origin", "main")
	e.Git(e.Origin, "branch", "-D", "feat")
	gh.mu.Lock()
	gh.prs = fmt.Sprintf(`[{"merged_at":"2026-01-01T00:00:00Z","head":{"ref":"feat","sha":%q,"repo":{"full_name":"acme/api"}}}]`, tip)
	gh.mu.Unlock()

	r.clk.BlockUntil(3, time.Second)
	r.clk.Advance(45 * time.Minute)
	r.waitFor(t, "feat deleted locally", func() bool {
		_, err := e.R.Run(context.Background(), dest, "rev-parse", "--verify", "-q", "refs/heads/feat")
		return err != nil
	})
	trash := e.Git(dest, "for-each-ref", "refs/repo-keeper/trash/")
	if !strings.Contains(trash, tip) {
		t.Fatalf("trash ref missing: %q", trash)
	}
	data, _ := os.ReadFile(filepath.Join(r.d.StateDir, "audit.jsonl"))
	if !strings.Contains(string(data), `"action":"deleted"`) || !strings.Contains(string(data), "pr-merged-tip-matches") {
		t.Fatalf("journal=%s", data)
	}
}

func TestDaemon_TokenNeverLogged(t *testing.T) {
	e := gitxtest.New(t)
	gh := &fakeGH{}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	gh.set(ghRepo("acme/api", e.Origin, false))
	var buf syncBuf
	log := newTestLog(&buf)
	dir := t.TempDir()
	root := filepath.Join(dir, "code")
	cfg := filepath.Join(dir, "config.toml")
	writeCfg(t, cfg, acctCfg(root, srv.URL, tokenFile(t), ""))
	d := &Daemon{ConfigPath: cfg, StateDir: filepath.Join(dir, "state"), Runner: e.R, Log: log.logger, Redactor: log.red, Secrets: &secrets.Mem{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rs, _ := d.Store.ListRepos(context.Background()); len(rs) == 1 && rs[0].LastReason == "cloned" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if strings.Contains(buf.String(), ghToken) {
		t.Fatalf("token leaked into logs:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "daemon started") {
		t.Fatal("expected log output")
	}
}

// fakeGitLab serves one nested-group project whose clone URL is a local bare repo.
type fakeGitLab struct {
	mu      sync.Mutex
	cloneTo string
	mrs     string
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+ghToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
		return
	}
	switch r.URL.EscapedPath() {
	case "/api/v4/user":
		_, _ = w.Write([]byte(`{"username":"gina"}`))
	case "/api/v4/projects":
		_, _ = fmt.Fprintf(w, `[{"id":7,"path_with_namespace":"acme/platform/infra/terraform","default_branch":"main","http_url_to_repo":%q,"ssh_url_to_repo":%q,"visibility":"private"}]`, f.cloneTo, f.cloneTo)
	case "/api/v4/projects/acme%2Fplatform%2Finfra%2Fterraform/merge_requests":
		_, _ = w.Write([]byte(f.mrs))
	default:
		http.NotFound(w, r)
	}
}

func TestDaemon_GitLab_NestedGroups_ClonedAndSquashCleanedViaMR(t *testing.T) {
	e := gitxtest.New(t)
	e.Git(e.Work, "checkout", "-q", "-b", "feat")
	e.Commit(e.Work, "f1.txt", "1", "one")
	e.Commit(e.Work, "f2.txt", "2", "two")
	e.Git(e.Work, "push", "-q", "-u", "origin", "feat")
	e.Git(e.Work, "checkout", "-q", "main")

	gl := &fakeGitLab{cloneTo: e.Origin}
	srv := httptest.NewServer(gl)
	defer srv.Close()
	root := filepath.Join(t.TempDir(), "code")
	cfg := strings.ReplaceAll(acctCfg(root, srv.URL, tokenFile(t), `cleanup = "auto"`+"\n"+`include = ["acme/**"]`), `provider = "github"`, `provider = "gitlab"`)
	r := start(t, e, cfg)

	dest := filepath.Join(root, "gitlab", "acme", "platform", "infra", "terraform")
	r.waitFor(t, "nested clone", func() bool { rs := r.repos(t); return len(rs) == 1 && rs[0].LastReason == "cloned" })
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("nested working tree missing: %v", err)
	}
	if a := r.account(t); a.Status != "ok" || a.Login != "gina" || a.Repos != 1 {
		t.Fatalf("account=%+v", a)
	}

	e.Git(dest, "branch", "--track", "feat", "origin/feat")
	tip := e.Git(dest, "rev-parse", "feat")
	e.Git(e.Work, "merge", "-q", "--squash", "feat")
	e.Git(e.Work, "commit", "-q", "-m", "squash feat")
	e.Git(e.Work, "push", "-q", "origin", "main")
	e.Git(e.Origin, "branch", "-D", "feat")
	gl.mu.Lock()
	gl.mrs = fmt.Sprintf(`[{"source_branch":"feat","sha":%q,"source_project_id":7,"target_project_id":7,"merged_at":"2026-01-01T00:00:00Z"}]`, tip)
	gl.mu.Unlock()

	r.clk.BlockUntil(3, time.Second)
	r.clk.Advance(45 * time.Minute)
	r.waitFor(t, "feat deleted via merge request evidence", func() bool {
		_, err := e.R.Run(context.Background(), dest, "rev-parse", "--verify", "-q", "refs/heads/feat")
		return err != nil
	})
	if data, _ := os.ReadFile(filepath.Join(r.d.StateDir, "audit.jsonl")); !strings.Contains(string(data), "pr-merged-tip-matches") {
		t.Fatalf("journal=%s", data)
	}
}

func TestCredFor_UsesTheProvidersUsername(t *testing.T) {
	c := credFor("https://gitlab.example.com/a/b.git", "oauth2", secrets.New("tok"))
	if c == nil || c.Username != "oauth2" || c.Host != "gitlab.example.com" {
		t.Fatalf("c=%+v", c)
	}
	if credFor("git@host:a/b.git", "oauth2", secrets.New("tok")) != nil || credFor("/local/path", "u", secrets.New("tok")) != nil || credFor("https://h/x", "u", secrets.Token{}) != nil {
		t.Fatal("non-https remotes and empty tokens must not get a credential")
	}
}
