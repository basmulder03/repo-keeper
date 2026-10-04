// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const usageText = `Usage: repo-keeper <command> [flags]

Commands:
  init                          write a starter configuration file
  config validate               check the configuration file
  start | stop | restart        run the daemon detached
  accounts add|list|check|rm    manage platform logins
  sync <repo>                   fetch and fast-forward
  restore <repo> <branch>       bring back a branch
  version                       print version

Run "repo-keeper <command> -h" for flags.
`

func TestParseCommands_UsageBlock(t *testing.T) {
	got := parseCommands(usageText)
	if len(got) != 7 || got[3].Name != "accounts add|list|check|rm" || got[3].Description != "manage platform logins" || got[4].Name != "sync <repo>" {
		t.Fatalf("got %+v", got)
	}
}

func TestExpandAndWords(t *testing.T) {
	for in, want := range map[string][]string{
		"init":                       {"init"},
		"start | stop | restart":     {"start", "stop", "restart"},
		"accounts add|list|check|rm": {"accounts add", "accounts list", "accounts check", "accounts rm"},
		"config validate":            {"config validate"},
		"restore <repo> <branch>":    {"restore <repo> <branch>"},
	} {
		if got := expand(in); !reflect.DeepEqual(got, want) {
			t.Errorf("expand(%q) = %v, want %v", in, got, want)
		}
	}
	if got := words("restore <repo> <branch>"); !reflect.DeepEqual(got, []string{"restore"}) {
		t.Errorf("words = %v", got)
	}
}

func TestParseCoverage_TotalsLikeGoToolCover(t *testing.T) {
	profile := `mode: atomic
example.com/m/internal/a/a.go:1.1,2.2 4 1
example.com/m/internal/a/a.go:3.1,4.2 6 0
example.com/m/internal/b/b.go:1.1,2.2 10 3
example.com/m/internal/gitxtest/x.go:1.1,2.2 50 0
`
	c, err := parseCoverage(bufio.NewScanner(strings.NewReader(profile)), "example.com/m")
	if err != nil {
		t.Fatal(err)
	}
	if c.Percent != 70 || len(c.ByPkg) != 2 || c.ByPkg[0].Pkg != "internal/a" || c.ByPkg[0].Percent != 40 || c.ByPkg[1].Percent != 100 {
		t.Fatalf("%+v (test helpers must be excluded, statements weighted)", c)
	}
	for _, bad := range []string{"", "mode: set\n", "mode: set\nnot a profile line\n"} {
		if _, err := parseCoverage(bufio.NewScanner(strings.NewReader(bad)), "m"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseGoMod(t *testing.T) {
	m := parseGoMod("module example.com/m\n\ngo 1.26\n\nrequire (\n\ta.org/x v1.2.3\n\tb.org/y v0.1.0 // indirect\n)\n\nrequire c.org/z v2.0.0\n")
	if m.Module != "example.com/m" || len(m.Direct) != 2 || m.Direct[0] != "a.org/x v1.2.3" || m.Direct[1] != "c.org/z v2.0.0" || len(m.Indirect) != 1 {
		t.Fatalf("%+v", m)
	}
}

func TestLinker_RewritesAndRefuses(t *testing.T) {
	var problems []string
	l := linker{
		bySource: map[string]string{"docs/INSTALL.md": "install", "README.md": "index", "SECURITY.md": "security"},
		repoURL:  "https://github.com/o/r", branch: "main", problems: &problems, from: "docs/BETA.md",
	}
	for in, want := range map[string]string{
		"INSTALL.md":                       "install.html",
		"INSTALL.md#verify":                "install.html#verify",
		"../README.md":                     "index.html",
		"../SECURITY.md":                   "security.html",
		"#local":                           "#local",
		"https://example.com/x.md":         "https://example.com/x.md",
		"mailto:a@b.c":                     "mailto:a@b.c",
		"../scripts/check-version.sh":      "https://github.com/o/r/blob/main/scripts/check-version.sh",
		"../internal/provider/":            "https://github.com/o/r/tree/main/internal/provider",
		"../.github/workflows/release.yml": "https://github.com/o/r/blob/main/.github/workflows/release.yml",
	} {
		if got := l.rewrite(in); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", in, got, want)
		}
	}
	if len(problems) != 0 {
		t.Fatalf("unexpected problems %v", problems)
	}
	l.rewrite("MISSING.md")
	l.rewrite("../../outside.md")
	if len(problems) != 2 || !strings.Contains(problems[0], "not published") || !strings.Contains(problems[1], "leaves the repository") {
		t.Fatalf("problems=%v", problems)
	}
}

func TestMarkdown_TablesScrollAndLinksAreRewritten(t *testing.T) {
	var problems []string
	l := &linker{bySource: map[string]string{"docs/A.md": "a"}, repoURL: "https://github.com/o/r", branch: "main", problems: &problems, from: "docs/B.md"}
	h, err := markdown([]byte("# T\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nsee [A](A.md#x) and <script>alert(1)</script>\n"), l)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h, `class="table-wrap"`) || !strings.Contains(h, `href="a.html#x"`) || strings.Contains(h, "<script>") {
		t.Fatalf("html=%s", h)
	}
}

// fakeBinary stands in for repo-keeper: usage text, per-command flag help, and a starter config for init.
func fakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  "") printf '%s\n' 'Usage: repo-keeper <command> [flags]' '' 'Commands:' '  init                          write a starter configuration file' '  version                       print version' '' 'Run "repo-keeper <command> -h" for flags.' ;;
  init) if [ "$2" = "--config" ]; then printf '[general]\nroot = "/home/you/code"\n' > "$3"; else echo 'Usage of init:'; fi ;;
  version) echo "repo-keeper test" ;;
esac
`
	p := filepath.Join(dir, "repo-keeper")
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // a test fixture script must be executable
		t.Fatal(err)
	}
	return p
}

// TestBuild_RealRepository builds the whole site from this repository: every Markdown link must resolve, every page
// must be written, and the generated pages must carry what the binary and the tree say.
func TestBuild_RealRepository(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	b := &build{
		Repo: repo, Out: t.TempDir(), Bin: fakeBinary(t), RepoURL: "https://github.com/basmulder03/repo-keeper", Branch: "main",
		Version: "9.9.9-test", Commit: "abc1234", Date: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
	}
	if err := b.generate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range pageList() {
		data, err := os.ReadFile(filepath.Join(b.Out, p.Slug+".html")) //nolint:gosec // temp output
		if err != nil {
			t.Fatalf("%s not written: %v", p.Slug, err)
		}
		h := string(data)
		if !strings.Contains(h, "<title>"+p.Title+" · repo-keeper</title>") || !strings.Contains(h, "v9.9.9-test") || !strings.Contains(h, `href="#main"`) {
			t.Errorf("%s: layout incomplete", p.Slug)
		}
	}
	read := func(slug string) string {
		d, _ := os.ReadFile(filepath.Join(b.Out, slug+".html")) //nolint:gosec // temp output
		return string(d)
	}
	if h := read("cli"); !strings.Contains(h, "<code>init</code>") || !strings.Contains(h, "Usage of init:") || !strings.Contains(h, "takes no flags") {
		t.Errorf("cli page: %.400s", h)
	}
	if h := read("config"); !strings.Contains(h, `root = &#34;/home/you/code&#34;`) {
		t.Errorf("config page: %.400s", h)
	}
	if h := read("stats"); !strings.Contains(h, "test functions") || !strings.Contains(h, "gitea") {
		t.Errorf("stats page: %.400s", h)
	}
	for _, f := range []string{"site.css", "icon.svg", "404.html", ".nojekyll"} {
		if _, err := os.Stat(filepath.Join(b.Out, f)); err != nil {
			t.Errorf("%s missing", f)
		}
	}
}

func TestBuild_BrokenLinkFailsTheBuild(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range pageList() {
		if p.Source != "" {
			write(p.Source, "# "+p.Title+"\n")
		}
	}
	write("README.md", "# Home\n\n[gone](docs/NOPE.md)\n")
	write("go.mod", "module example.com/m\n\ngo 1.26\n")
	write("VERSION", "1.0.0\n")
	write("scripts/install.sh", "VERSION=\"${REPO_KEEPER_VERSION:-@VERSION@}\"\n")
	b := &build{Repo: repo, Out: t.TempDir(), Bin: fakeBinary(t), RepoURL: "https://github.com/o/r", Branch: "main", Version: "1.0.0", Commit: "x", Date: time.Now()}
	if err := b.generate(); err == nil || !strings.Contains(err.Error(), "NOPE.md") {
		t.Fatalf("err=%v", err)
	}
}

func realBuild(t *testing.T, releases string) *build {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return &build{
		Repo: repo, Out: t.TempDir(), Bin: fakeBinary(t), Releases: releases, RepoURL: "https://github.com/basmulder03/repo-keeper", Branch: "main",
		Version: "9.9.9-test", Commit: "abc1234", Date: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
	}
}

func TestInstall_NoPublishedRelease_PinsVERSIONAndSaysSo(t *testing.T) {
	b := realBuild(t, "")
	if err := b.generate(); err != nil {
		t.Fatal(err)
	}
	page, _ := os.ReadFile(filepath.Join(b.Out, "install.html")) //nolint:gosec // temp output
	script, _ := os.ReadFile(filepath.Join(b.Out, "install.sh")) //nolint:gosec // temp output
	h, s := string(page), string(script)
	if !strings.Contains(h, "No release is published yet") || !strings.Contains(h, "v9.9.9-test") {
		t.Errorf("page must be honest that nothing is published: %.300s", h)
	}
	if !strings.Contains(s, `VERSION="${REPO_KEEPER_VERSION:-9.9.9-test}"`) {
		t.Errorf("script is not pinned")
	}
	if !strings.Contains(s, `@VERSION@|"") die`) {
		t.Errorf("the unpinned-copy guard must keep its placeholder, or the pinned version would be rejected")
	}
	sum := sha256.Sum256(script)
	if !strings.Contains(h, hex.EncodeToString(sum[:])) {
		t.Errorf("the page must show the SHA-256 of the script it serves")
	}
}

func TestInstall_PinsNewestPublishedRelease_IgnoresDrafts(t *testing.T) {
	rel := filepath.Join(t.TempDir(), "releases.json")
	data := `[
	 {"tag_name":"v2.0.0-beta.9","draft":true,"published_at":"2026-10-05T00:00:00Z","html_url":"https://x/9","assets":[]},
	 {"tag_name":"v2.0.0-beta.2","draft":false,"prerelease":true,"published_at":"2026-10-02T00:00:00Z","html_url":"https://x/2","assets":[{"name":"repo-keeper_2.0.0-beta.2_linux_amd64.tar.gz","browser_download_url":"https://x/a.tgz","download_count":7},{"name":"checksums.txt.sig","browser_download_url":"https://x/s","download_count":1}]},
	 {"tag_name":"v2.0.0-beta.1","draft":false,"published_at":"2026-10-01T00:00:00Z","html_url":"https://x/1","assets":[]}]`
	if err := os.WriteFile(rel, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	b := realBuild(t, rel)
	if err := b.generate(); err != nil {
		t.Fatal(err)
	}
	read := func(f string) string {
		d, _ := os.ReadFile(filepath.Join(b.Out, f)) //nolint:gosec // temp output
		return string(d)
	}
	if h := read("install.html"); !strings.Contains(h, "v2.0.0-beta.2") || strings.Contains(h, "v2.0.0-beta.9") || strings.Contains(h, "No release is published yet") {
		t.Errorf("must pin the newest PUBLISHED release: %.400s", h)
	}
	if s := read("install.sh"); !strings.Contains(s, `:-2.0.0-beta.2}`) {
		t.Errorf("script pinned to the wrong release")
	}
	c := read("changelog.html")
	if !strings.Contains(c, `href="https://x/a.tgz"`) || strings.Contains(c, "https://x/s\"") || !strings.Contains(c, "pre-release") || strings.Contains(c, "v2.0.0-beta.9") {
		t.Errorf("downloads table wrong (drafts hidden, signature files folded away): %.600s", c)
	}
	if s := read("stats.html"); !strings.Contains(s, "2 published releases") || !strings.Contains(s, "8 file downloads") {
		t.Errorf("release statistics wrong: %.500s", s)
	}
}
