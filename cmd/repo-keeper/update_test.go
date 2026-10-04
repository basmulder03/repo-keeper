// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/update"
	"github.com/basmulder03/repo-keeper/internal/update/apply"
)

const fakeELF = "\x7fELF-fake-"

type stubVerifier struct{ err error }

func (s stubVerifier) Verify(context.Context, []byte, []byte, []byte, string, string) error {
	return s.err
}

type releaseSite struct {
	srv      *httptest.Server
	archive  atomic.Value // []byte served at the archive URL
	api      atomic.Int32 // hits on /releases
	download atomic.Int32 // hits on the archive itself
	body     string
}

func tarball(t *testing.T, tag string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"repo-keeper": fakeELF + tag, "repo-keeper-tray": fakeELF + "tray-" + tag} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newReleaseSite serves one API and download host with releases v0.1.0-beta.5 (the one "installed") and v0.1.0-beta.6.
func newReleaseSite(t *testing.T, notes6 string) *releaseSite {
	t.Helper()
	s := &releaseSite{}
	arch := map[string][]byte{"v0.1.0-beta.5": tarball(t, "v5"), "v0.1.0-beta.6": tarball(t, "v6")}
	s.archive.Store(arch["v0.1.0-beta.6"])
	mux := http.NewServeMux()
	asset := func(tag, file string) string {
		return fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/o/r/releases/download/%s/%s"}`, file, "%s", tag, file)
	}
	_ = asset
	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, _ *http.Request) {
		s.api.Add(1)
		rel := func(tag, notes string) string {
			name := "repo-keeper_" + strings.TrimPrefix(tag, "v") + "_linux_amd64.tar.gz"
			as := ""
			for _, f := range []string{name, "checksums.txt", "checksums.txt.sig", "checksums.txt.pem"} {
				as += fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/o/r/releases/download/%s/%s"},`, f, s.srv.URL, tag, f)
			}
			return fmt.Sprintf(`{"tag_name":%q,"prerelease":true,"published_at":"2026-10-04T10:00:00Z","html_url":"https://github.com/o/r/releases/tag/%s","body":%q,"assets":[%s]}`, tag, tag, notes, strings.TrimSuffix(as, ","))
		}
		_, _ = fmt.Fprintf(w, `[%s,%s,{"tag_name":"v0.1.0-beta.7","draft":true}]`, rel("v0.1.0-beta.6", notes6), rel("v0.1.0-beta.5", "older"))
	})
	for tag := range arch {
		tag := tag
		name := "repo-keeper_" + strings.TrimPrefix(tag, "v") + "_linux_amd64.tar.gz"
		base := "/o/r/releases/download/" + tag + "/"
		mux.HandleFunc(base+name, func(w http.ResponseWriter, _ *http.Request) {
			s.download.Add(1)
			if tag == "v0.1.0-beta.6" {
				_, _ = w.Write(s.archive.Load().([]byte))
				return
			}
			_, _ = w.Write(arch[tag])
		})
		mux.HandleFunc(base+"checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
			sum := sha256.Sum256(arch[tag]) // always the genuine archive's hash, whatever the archive endpoint serves
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		})
		mux.HandleFunc(base+"checksums.txt.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("sig")) })
		mux.HandleFunc(base+"checksums.txt.pem", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pem")) })
		mux.HandleFunc("/repos/o/r/commits/"+tag, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"sha":"deadbeefcafe"}`)) })
	}
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"workflow_runs":[{"name":"release","html_url":"https://github.com/o/r/actions/runs/7"}]}`))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

type updRig struct {
	a    *app
	out  *bytes.Buffer
	err  *bytes.Buffer
	site *releaseSite
	exe  string
}

func newUpdRig(t *testing.T, running string, verifier apply.Verifier) *updRig {
	t.Helper()
	old := version
	version = running
	t.Cleanup(func() { version = old })
	site := newReleaseSite(t, "### Added\n- shiny things")
	dir := t.TempDir()
	exe := filepath.Join(dir, "repo-keeper")
	if err := os.WriteFile(exe, []byte(fakeELF+"v5"), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo-keeper-tray"), []byte(fakeELF+"tray-v5"), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	a, out, errb := newApp(gitxtest.New(t))
	a.in = strings.NewReader("y\n")
	a.upd = updateEnv{
		exe:         func() (string, error) { return exe, nil },
		probe:       update.Probe{Exists: func(string) bool { return false }},
		interactive: func() bool { return true },
		source: func() (apply.Source, error) {
			hc, err := httpx.New(httpx.Config{UserAgent: "repo-keeper/test", Limiter: ratelimit.New(ratelimit.Config{Rate: 1000, Burst: 1000}, clock.Real{}, nil)})
			return apply.Source{Checker: update.Checker{HTTP: hc, API: site.srv.URL, Repo: "o/r"}, Download: site.srv.URL, Verifier: verifier, GOOS: "linux", GOARCH: "amd64"}, err
		},
	}
	return &updRig{a: a, out: out, err: errb, site: site, exe: exe}
}

func (r *updRig) run(args ...string) int {
	return r.a.run(context.Background(), append([]string{"update"}, args...))
}

func (r *updRig) read(p string) string {
	b, _ := os.ReadFile(p) //nolint:gosec // temp files
	return string(b)
}

func (r *updRig) untouched(t *testing.T) {
	t.Helper()
	if r.read(r.exe) != fakeELF+"v5" || r.read(r.exe+apply.PreviousSuffix) != "" {
		t.Fatalf("the installation changed: %q previous=%q", r.read(r.exe), r.read(r.exe+apply.PreviousSuffix))
	}
}

func TestUpdate_Check_ShowsAllTheEvidence_ChangesNothing(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	if code := r.run(); code != 0 {
		t.Fatalf("code=%d err=%s out=%s", code, r.err, r.out)
	}
	out := r.out.String()
	for _, want := range []string{
		"installed:  0.1.0-beta.5", "(tarball install at", "target:     0.1.0-beta.6", "[pre-release]", "release:    https://github.com/o/r/releases/tag/v0.1.0-beta.6",
		"shiny things", "asset:      " + r.site.srv.URL + "/o/r/releases/download/v0.1.0-beta.6/repo-keeper_0.1.0-beta.6_linux_amd64.tar.gz",
		"sha256:     expected ", "signature:  verified", "identity: https://github.com/o/r/.github/workflows/release.yml@refs/tags/v0.1.0-beta.6",
		"issuer:   https://token.actions.githubusercontent.com", "commit deadbeefcafe", "https://github.com/o/r/actions/runs/7",
		"Verify it yourself", "cosign verify-blob checksums.txt", "sha256sum --check --ignore-missing checksums.txt", "gh attestation verify repo-keeper_0.1.0-beta.6_linux_amd64.tar.gz --repo o/r",
		"To install it: repo-keeper update --apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("evidence lacks %q", want)
		}
	}
	if r.site.download.Load() != 0 {
		t.Fatal("--check must not download the archive")
	}
	r.untouched(t)
}

func TestUpdate_Check_UpToDate_SaysSo_AndFetchesNothingElse(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.6", stubVerifier{})
	if code := r.run("--check"); code != 0 || !strings.Contains(r.out.String(), "up to date") || strings.Contains(r.out.String(), "Verify it yourself") {
		t.Fatalf("code=%d out=%s", code, r.out)
	}
	r = newUpdRig(t, "0.1.0-beta.9", stubVerifier{})
	if code := r.run(); code != 0 || !strings.Contains(r.out.String(), "newer than the newest published release") {
		t.Fatalf("a newer local build must not be 'updated' backwards: %d %s", code, r.out)
	}
}

func TestUpdate_Check_MarksSecurityReleases(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	r.site.body = ""
	r.site.srv.Close()
	r.site = newReleaseSite(t, "### Security\n- fixed CVE-2026-9999")
	r.a.upd.source = func() (apply.Source, error) {
		hc, _ := httpx.New(httpx.Config{UserAgent: "repo-keeper/test", Limiter: ratelimit.New(ratelimit.Config{Rate: 1000, Burst: 1000}, clock.Real{}, nil)})
		return apply.Source{Checker: update.Checker{HTTP: hc, API: r.site.srv.URL, Repo: "o/r"}, Download: r.site.srv.URL, Verifier: stubVerifier{}, GOOS: "linux", GOARCH: "amd64"}, nil
	}
	if code := r.run(); code != 0 || !strings.Contains(r.out.String(), "[SECURITY FIX]") || !strings.Contains(r.out.String(), "CVE-2026-9999") {
		t.Fatalf("code=%d out=%s", code, r.out)
	}
}

func TestUpdate_Check_SignatureProblemIsShownNotHidden(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{err: apply.ErrNoCosign})
	if code := r.run(); code != 0 || !strings.Contains(r.out.String(), "signature:  NOT verified: cosign is not installed") {
		t.Fatalf("code=%d out=%s", code, r.out)
	}
}

func TestUpdate_Apply_HappyPath_InstallsKeepsPreviousNeverRestarts(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	if code := r.run("--apply"); code != 0 {
		t.Fatalf("code=%d err=%s out=%s", code, r.err, r.out)
	}
	out := r.out.String()
	for _, want := range []string{"Install 0.1.0-beta.6 over 0.1.0-beta.5 at ", "[y/N]", "sha256 computed ", "match", "installed 0.1.0-beta.6", "repo-keeper update --rollback", "keeps the old version until it is restarted"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if r.read(r.exe) != fakeELF+"v6" || r.read(r.exe+apply.PreviousSuffix) != fakeELF+"v5" || r.read(filepath.Join(filepath.Dir(r.exe), "repo-keeper-tray")) != fakeELF+"tray-v6" {
		t.Fatalf("files wrong: %q / %q", r.read(r.exe), r.read(r.exe+apply.PreviousSuffix))
	}
}

func TestUpdate_Apply_DefaultIsNo(t *testing.T) {
	for name, answer := range map[string]string{"empty": "\n", "no": "n\n", "eof": "", "garbage": "sure thing\n", "yes-ish": "yep\n"} {
		r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
		r.a.in = strings.NewReader(answer)
		if code := r.run("--apply"); code != 1 || !strings.Contains(r.out.String(), "not installed") {
			t.Errorf("%s: code=%d out=%s", name, code, r.out)
		}
		if r.site.download.Load() != 0 {
			t.Errorf("%s: the archive was downloaded without consent", name)
		}
		r.untouched(t)
	}
}

func TestUpdate_Apply_NeverWithoutATerminal(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	r.a.upd.interactive = func() bool { return false }
	r.a.in = strings.NewReader("y\n") // even a piped "y" must not count
	if code := r.run("--apply"); code != 1 || !strings.Contains(r.err.String(), "needs a terminal") {
		t.Fatalf("code=%d err=%s", code, r.err)
	}
	if r.site.download.Load() != 0 {
		t.Fatal("downloaded without a terminal")
	}
	r.untouched(t)
	r2 := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	r2.a.upd.interactive = func() bool { return false }
	if code := r2.run("--rollback"); code != 1 || !strings.Contains(r2.err.String(), "needs a terminal") {
		t.Fatalf("rollback without a terminal: %d %s", code, r2.err)
	}
}

func TestUpdate_Apply_ManagedInstalls_AreReferredToTheirManager_BeforeAnyNetwork(t *testing.T) {
	for name, tc := range map[string]struct {
		exe    string
		exists map[string]bool
		want   string
	}{
		"nix":       {"/nix/store/x-repo-keeper/bin/repo-keeper", nil, "nix profile upgrade"},
		"deb":       {"/usr/bin/repo-keeper", map[string]bool{"/etc/debian_version": true}, "apt install"},
		"rpm":       {"/usr/bin/repo-keeper", map[string]bool{"/etc/fedora-release": true}, "dnf install"},
		"homebrew":  {"/opt/homebrew/Cellar/repo-keeper/1/bin/repo-keeper", nil, "brew upgrade"},
		"container": {"/home/u/.local/bin/repo-keeper", map[string]bool{"/.dockerenv": true}, "newer image"},
	} {
		r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
		r.a.upd.exe = func() (string, error) { return tc.exe, nil }
		r.a.upd.probe = update.Probe{Exists: func(p string) bool { return tc.exists[p] }}
		if code := r.run("--apply"); code != 1 || !strings.Contains(r.err.String(), tc.want) {
			t.Errorf("%s: code=%d err=%s", name, code, r.err)
		}
		if r.site.api.Load() != 0 || r.site.download.Load() != 0 {
			t.Errorf("%s: it must refuse before talking to anyone", name)
		}
		if code := r.run("--rollback"); code != 1 || !strings.Contains(r.err.String(), tc.want) {
			t.Errorf("%s rollback: code=%d", name, code)
		}
	}
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{}) // --check on a managed install still informs, with the manager's command
	r.a.upd.exe = func() (string, error) { return "/nix/store/x-repo-keeper/bin/repo-keeper", nil }
	if code := r.run("--check"); code != 0 || !strings.Contains(r.out.String(), "managed by nix") || !strings.Contains(r.out.String(), "nix profile upgrade") || strings.Contains(r.out.String(), "To install it: repo-keeper update --apply") {
		t.Fatalf("code=%d out=%s", code, r.out)
	}
}

func TestUpdate_Apply_RefusesUnverifiedSignature_BeforeAskingAnything(t *testing.T) {
	for name, v := range map[string]apply.Verifier{"no cosign": stubVerifier{err: apply.ErrNoCosign}, "bad signature": stubVerifier{err: errors.New("identity mismatch")}, "no verifier": nil} {
		r := newUpdRig(t, "0.1.0-beta.5", v)
		if code := r.run("--apply"); code != 1 || !strings.Contains(r.err.String(), "refusing to install: the release signature is") {
			t.Errorf("%s: code=%d err=%s", name, code, r.err)
		}
		if strings.Contains(r.out.String(), "[y/N]") || r.site.download.Load() != 0 {
			t.Errorf("%s: it must not even ask", name)
		}
		r.untouched(t)
	}
}

func TestUpdate_Apply_TamperedArchive_IsRefusedAfterConsent_NothingInstalled(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	r.site.archive.Store(tarball(t, "EVIL"))
	if code := r.run("--apply"); code != 1 || !strings.Contains(r.err.String(), "SHA-256 mismatch") {
		t.Fatalf("code=%d err=%s", code, r.err)
	}
	r.untouched(t)
}

func TestUpdate_Apply_DevelopmentBuildHasNothingToUpdateFrom(t *testing.T) {
	for _, v := range []string{"dev", "0166000-dirty", ""} {
		r := newUpdRig(t, v, stubVerifier{})
		if code := r.run("--apply"); code != 1 || !strings.Contains(r.err.String(), "development build") {
			t.Errorf("%q: code=%d err=%s", v, code, r.err)
		}
		if r.site.api.Load() != 0 {
			t.Errorf("%q: no network for a development build", v)
		}
	}
}

func TestUpdate_Apply_ExplicitVersion_AllowsDowngradeButSaysSoLoudly(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.6", stubVerifier{})
	if code := r.run("--apply"); code != 0 || !strings.Contains(r.out.String(), "up to date") || r.site.download.Load() != 0 {
		t.Fatalf("without --version a newer-or-equal build is never changed: %d %s", code, r.out)
	}
	r = newUpdRig(t, "0.1.0-beta.6", stubVerifier{})
	if code := r.run("--apply", "--version", "0.1.0-beta.5"); code != 0 {
		t.Fatalf("code=%d err=%s out=%s", code, r.err, r.out)
	}
	if !strings.Contains(r.out.String(), "DOWNGRADE to 0.1.0-beta.5 over 0.1.0-beta.6") || !strings.Contains(r.out.String(), "[OLDER THAN WHAT YOU RUN]") || r.read(r.exe) != fakeELF+"v5" {
		t.Fatalf("out=%s exe=%q", r.out, r.read(r.exe))
	}
}

func TestUpdate_Flags_AreChecked(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	for _, args := range [][]string{{"--check", "--apply"}, {"--apply", "--rollback"}, {"--rollback", "--version", "0.1.0"}, {"extra"}} {
		if code := r.run(args...); code != 2 || !strings.Contains(r.err.String(), "usage: repo-keeper update") {
			t.Errorf("%v: code=%d", args, code)
		}
	}
	if code := r.run("--apply", "--version", "banana"); code != 2 {
		t.Errorf("a junk version: code=%d", code)
	}
	if code := r.run("--apply", "--version", "9.9.9"); code != 1 || !strings.Contains(r.err.String(), "no published release 9.9.9") {
		t.Errorf("an unknown version: code=%d err=%s", code, r.err)
	}
	if code := r.run("--apply", "--version", "0.1.0-beta.7"); code != 1 || !strings.Contains(r.err.String(), "no published release") {
		t.Errorf("a draft must never be offered: code=%d", code)
	}
	r.untouched(t)
}

func TestUpdate_UnsupportedPlatform_NoSignedBuild(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	base := r.a.upd.source
	r.a.upd.source = func() (apply.Source, error) {
		s, err := base()
		s.GOOS, s.GOARCH = "darwin", "arm64"
		return s, err
	}
	if code := r.run("--check"); code != 1 || !strings.Contains(r.err.String(), "no signed release is published for darwin/arm64") {
		t.Fatalf("code=%d err=%s", code, r.err)
	}
}

func TestUpdate_Rollback_RestoresAndCanBeUndone(t *testing.T) {
	r := newUpdRig(t, "0.1.0-beta.5", stubVerifier{})
	if code := r.run("--rollback"); code != 1 || !strings.Contains(r.err.String(), "no previous version") {
		t.Fatalf("rollback with nothing kept: %d %s", code, r.err)
	}
	if r.run("--apply") != 0 {
		t.Fatalf("apply: %s", r.err)
	}
	r.a.in = strings.NewReader("y\n")
	if code := r.run("--rollback"); code != 0 || r.read(r.exe) != fakeELF+"v5" || !strings.Contains(r.out.String(), "restored") {
		t.Fatalf("code=%d exe=%q err=%s", code, r.read(r.exe), r.err)
	}
	r.a.in = strings.NewReader("n\n")
	if code := r.run("--rollback"); code != 1 || r.read(r.exe) != fakeELF+"v5" {
		t.Fatalf("a refused rollback must change nothing: %d %q", code, r.read(r.exe))
	}
}

func TestUpdateNotice_Wording(t *testing.T) {
	for name, tc := range map[string]struct {
		st      update.State
		current string
		want    string
	}{
		"nothing known":    {update.State{}, "0.1.0-beta.5", ""},
		"up to date":       {update.State{Latest: "0.1.0-beta.5"}, "0.1.0-beta.5", ""},
		"newer":            {update.State{Latest: "0.1.0-beta.6"}, "0.1.0-beta.5", "update available: 0.1.0-beta.6 (you run 0.1.0-beta.5). Review it with: repo-keeper update"},
		"newer, security":  {update.State{Latest: "0.1.0-beta.6", Urgent: true}, "0.1.0-beta.5", "it contains a SECURITY FIX"},
		"development":      {update.State{Latest: "9.9.9"}, "dev", ""},
		"older than local": {update.State{Latest: "0.1.0-beta.4"}, "0.1.0-beta.5", ""},
	} {
		if got := updateNotice(tc.st, tc.current); (tc.want == "" && got != "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestDoctor_ReportsNewerReleaseAndUpToDate(t *testing.T) {
	_, stateDir, _ := isolatedEnv(t)
	old := version
	version = "0.1.0-beta.5"
	t.Cleanup(func() { version = old })
	e := gitxtest.New(t)
	a, out, _ := newApp(e)
	if code := a.run(context.Background(), []string{"doctor"}); code != 0 || strings.Contains(out.String(), "update") {
		t.Fatalf("before any check doctor says nothing about updates: %d %s", code, out)
	}
	if err := update.WriteState(stateDir, update.State{Checked: clock.NewFake(time.Now()).Now(), Latest: "0.1.0-beta.6", Urgent: true}); err != nil {
		t.Fatal(err)
	}
	a, out, _ = newApp(e)
	if code := a.run(context.Background(), []string{"doctor"}); code != 0 || !strings.Contains(out.String(), "warn update available: 0.1.0-beta.6") || !strings.Contains(out.String(), "SECURITY FIX") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if err := update.WriteState(stateDir, update.State{Checked: time.Now(), Latest: "0.1.0-beta.5"}); err != nil {
		t.Fatal(err)
	}
	a, out, _ = newApp(e)
	if code := a.run(context.Background(), []string{"doctor"}); code != 0 || !strings.Contains(out.String(), "ok   update check: no newer release") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}
