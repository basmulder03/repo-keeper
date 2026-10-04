// SPDX-License-Identifier: Apache-2.0

package apply

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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/update"
)

const elf = "\x7fELF-fake-binary-"

type entry struct {
	name, body string
	typ        byte
	link       string
}

func archiveOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: typ, Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func goodArchive(t *testing.T, tag string) []byte {
	return archiveOf(t, entry{name: "LICENSE", body: "x"}, entry{name: "repo-keeper", body: elf + tag}, entry{name: "repo-keeper-tray", body: elf + "tray-" + tag}, entry{name: "systemd/repo-keeper.service", body: "[Service]"})
}

func TestInstall_ReplacesKeepsPreviousAndNeverTouchesOtherNames(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "repo-keeper")
	for f, body := range map[string]string{"repo-keeper": elf + "old", "repo-keeper-tray": elf + "tray-old", "unrelated": "keep me"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o755); err != nil { //nolint:gosec // test executables
			t.Fatal(err)
		}
	}
	res, err := Install(goodArchive(t, "new"), target)
	if err != nil || len(res.Replaced) != 2 || len(res.Previous) != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	read := func(f string) string { b, _ := os.ReadFile(filepath.Join(dir, f)); return string(b) } //nolint:gosec // temp files
	if read("repo-keeper") != elf+"new" || read("repo-keeper-tray") != elf+"tray-new" || read("repo-keeper.previous") != elf+"old" || read("repo-keeper-tray.previous") != elf+"tray-old" || read("unrelated") != "keep me" {
		t.Fatalf("files: %v", dirNames(t, dir))
	}
	if fi, _ := os.Stat(target); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 { // no POSIX mode bits on Windows
		t.Fatalf("mode %v", fi.Mode())
	}
	for _, n := range dirNames(t, dir) {
		if strings.HasPrefix(n, ".repo-keeper-update-") {
			t.Fatalf("temporary file left behind: %s", n)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestInstall_TrayOnlyReplacedWhenAlreadyInstalledNextToIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "repo-keeper")
	if err := os.WriteFile(target, []byte(elf+"old"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	if _, err := Install(goodArchive(t, "new"), target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "repo-keeper-tray")); err == nil {
		t.Fatal("an update must not install a tray the user never had")
	}
}

func TestInstall_HostileArchives_AreRefusedOrIgnored_NothingChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		entries []entry
		wantErr string
	}{
		"no daemon binary":   {[]entry{{name: "README.md", body: "x"}}, "does not contain repo-keeper"},
		"nested only":        {[]entry{{name: "pkg/repo-keeper", body: elf}}, "does not contain repo-keeper"},
		"traversal name":     {[]entry{{name: "../repo-keeper", body: elf}}, "does not contain repo-keeper"},
		"absolute name":      {[]entry{{name: "/usr/bin/repo-keeper", body: elf}}, "does not contain repo-keeper"},
		"symlink as daemon":  {[]entry{{name: "repo-keeper", typ: tar.TypeSymlink, link: "/etc/passwd"}}, "does not contain repo-keeper"},
		"hardlink as daemon": {[]entry{{name: "repo-keeper", typ: tar.TypeLink, link: "/etc/passwd"}}, "does not contain repo-keeper"},
		"not an executable":  {[]entry{{name: "repo-keeper", body: "#!/bin/sh\necho pwned"}}, "not a Linux executable"},
		"script as tray":     {[]entry{{name: "repo-keeper", body: elf}, {name: "repo-keeper-tray", body: "#!/bin/sh"}}, "not a Linux executable"},
		"empty":              {nil, "does not contain repo-keeper"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "repo-keeper")
			if err := os.WriteFile(target, []byte(elf+"old"), 0o755); err != nil { //nolint:gosec // test executable
				t.Fatal(err)
			}
			_, err := Install(archiveOf(t, tc.entries...), target)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if b, _ := os.ReadFile(target); string(b) != elf+"old" { //nolint:gosec // temp file
				t.Fatal("the installed binary changed despite the refusal")
			}
			if got := dirNames(t, dir); len(got) != 1 {
				t.Fatalf("stray files: %v", got)
			}
		})
	}
	if _, err := Install([]byte("not gzip"), filepath.Join(t.TempDir(), "repo-keeper")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestInstall_UnwritableDirectory_FailsBeforeChangingAnything(t *testing.T) {
	if os.Getuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs a non-root POSIX user")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "repo-keeper")
	if err := os.WriteFile(target, []byte(elf+"old"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // making the directory read-only is the point
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }() //nolint:gosec // restore so TempDir cleanup works
	_, err := Install(goodArchive(t, "new"), target)
	if err == nil || !strings.Contains(err.Error(), "package manager") {
		t.Fatalf("err=%v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != elf+"old" { //nolint:gosec // temp file
		t.Fatal("changed")
	}
}

func TestRollback_SwapsAndSwapsBack(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "repo-keeper")
	if err := os.WriteFile(target, []byte(elf+"v1"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	if _, err := Rollback(target); err == nil || !strings.Contains(err.Error(), "no previous version") {
		t.Fatalf("rollback without an update: %v", err)
	}
	if _, err := Install(goodArchive(t, "v2"), target); err != nil {
		t.Fatal(err)
	}
	read := func(f string) string { b, _ := os.ReadFile(f); return string(b) } //nolint:gosec // temp files
	if _, err := Rollback(target); err != nil || read(target) != elf+"v1" || read(target+PreviousSuffix) != elf+"v2" {
		t.Fatalf("after rollback: %q / %q err=%v", read(target), read(target+PreviousSuffix), err)
	}
	if _, err := Rollback(target); err != nil || read(target) != elf+"v2" {
		t.Fatalf("rolling back again must return to the newer version: %q", read(target))
	}
	if got := dirNames(t, dir); len(got) != 2 {
		t.Fatalf("leftovers: %v", got)
	}
}

// ---- gathering evidence ----

type fakeVerifier struct {
	err  error
	args []string
}

func (f *fakeVerifier) Verify(_ context.Context, _, _, _ []byte, identity, issuer string) error {
	f.args = []string{identity, issuer}
	return f.err
}

type site struct {
	srv     *httptest.Server
	archive []byte
	sums    string
	tag     string
}

func newSite(t *testing.T, tag string, mutate func(*site)) *site {
	t.Helper()
	s := &site{tag: tag, archive: goodArchive(t, tag)}
	if mutate != nil {
		mutate(s)
	}
	name := "repo-keeper_" + strings.TrimPrefix(tag, "v") + "_linux_amd64.tar.gz"
	if s.sums == "" {
		sum := sha256.Sum256(goodArchive(t, tag))
		s.sums = hex.EncodeToString(sum[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  other.deb\n"
	}
	mux := http.NewServeMux()
	base := "/o/r/releases/download/" + tag + "/"
	mux.HandleFunc(base+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(s.archive) })
	mux.HandleFunc(base+"checksums.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(s.sums)) })
	mux.HandleFunc(base+"checksums.txt.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("sig")) })
	mux.HandleFunc(base+"checksums.txt.pem", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pem")) })
	mux.HandleFunc("/repos/o/r/commits/"+tag, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"sha":"abc123def456"}`)) })
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head_sha") != "abc123def456" {
			http.Error(w, "sha", 400)
			return
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"name":"ci","html_url":"https://x/ci"},{"name":"release","html_url":"https://x/run/42"}]}`))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *site) release(t *testing.T, assetURLs map[string]string) update.Release {
	t.Helper()
	name := "repo-keeper_" + strings.TrimPrefix(s.tag, "v") + "_linux_amd64.tar.gz"
	r := update.Release{Tag: s.tag, URL: "https://x/rel"}
	for _, f := range []string{name, "checksums.txt", "checksums.txt.sig", "checksums.txt.pem"} {
		u := s.srv.URL + "/o/r/releases/download/" + s.tag + "/" + f
		if o, ok := assetURLs[f]; ok {
			u = o
		}
		if u != "" {
			r.Assets = append(r.Assets, update.Asset{Name: f, URL: u})
		}
	}
	return r
}

func (s *site) source(t *testing.T, v Verifier) Source {
	t.Helper()
	hc, err := httpx.New(httpx.Config{UserAgent: "repo-keeper/test", Limiter: ratelimit.New(ratelimit.Config{Rate: 1000, Burst: 1000}, clock.Real{}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	return Source{Checker: update.Checker{HTTP: hc, API: s.srv.URL, Repo: "o/r"}, Download: s.srv.URL, Verifier: v, GOOS: "linux", GOARCH: "amd64"}
}

func TestGather_CollectsEvidence_PinsIdentityToTheExactTag(t *testing.T) {
	s := newSite(t, "v1.2.3-beta.4", nil)
	fv := &fakeVerifier{}
	ev, err := s.source(t, fv).Gather(context.Background(), s.release(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Signature.Verified || ev.Version.String() != "1.2.3-beta.4" || ev.ArchiveName != "repo-keeper_1.2.3-beta.4_linux_amd64.tar.gz" {
		t.Fatalf("%+v", ev)
	}
	wantID := "https://github.com/o/r/.github/workflows/release.yml@refs/tags/v1.2.3-beta.4"
	if ev.Identity != wantID || fv.args[0] != wantID || fv.args[1] != Issuer || ev.Issuer != Issuer {
		t.Fatalf("identity/issuer not pinned to the exact tag: %v vs %q", fv.args, wantID)
	}
	if len(ev.ExpectedSHA) != 64 || ev.Commit != "abc123def456" || ev.RunURL != "https://x/run/42" || ev.Computed != "" {
		t.Fatalf("expected sha / provenance wrong: %+v", ev)
	}
}

func TestGather_SignatureFailureIsReported_NotFatal(t *testing.T) {
	s := newSite(t, "v1.0.0", nil)
	ev, err := s.source(t, &fakeVerifier{err: errors.New("bad cert")}).Gather(context.Background(), s.release(t, nil))
	if err != nil || ev.Signature.Verified || !strings.Contains(ev.Signature.Detail, "NOT verified: bad cert") {
		t.Fatalf("ev=%+v err=%v", ev.Signature, err)
	}
	ev, err = s.source(t, nil).Gather(context.Background(), s.release(t, nil))
	if err != nil || ev.Signature.Verified {
		t.Fatalf("no verifier must never count as verified: %+v", ev.Signature)
	}
	ev, err = s.source(t, &fakeVerifier{err: ErrNoCosign}).Gather(context.Background(), s.release(t, nil))
	if err != nil || ev.Signature.Verified || !strings.Contains(ev.Signature.Detail, "cosign is not installed") {
		t.Fatalf("%+v", ev.Signature)
	}
}

func TestGather_RefusesAssetsFromAnywhereElse(t *testing.T) {
	s := newSite(t, "v1.0.0", nil)
	name := "repo-keeper_1.0.0_linux_amd64.tar.gz"
	for label, override := range map[string]map[string]string{
		"another host":        {name: "https://evil.example/o/r/releases/download/v1.0.0/" + name},
		"another release":     {name: s.srv.URL + "/o/r/releases/download/v0.9.0/" + name},
		"another repository":  {"checksums.txt": s.srv.URL + "/evil/r/releases/download/v1.0.0/checksums.txt"},
		"subdirectory trick":  {name: s.srv.URL + "/o/r/releases/download/v1.0.0/../" + name},
		"query string":        {name: s.srv.URL + "/o/r/releases/download/v1.0.0/" + name + "?x=1"},
		"signature elsewhere": {"checksums.txt.sig": "https://evil.example/sig"},
		"missing signature":   {"checksums.txt.sig": ""},
	} {
		if _, err := s.source(t, &fakeVerifier{}).Gather(context.Background(), s.release(t, override)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestGather_ChecksumFileProblems(t *testing.T) {
	name := "repo-keeper_1.0.0_linux_amd64.tar.gz"
	for label, sums := range map[string]string{
		"archive not listed": strings.Repeat("a", 64) + "  other.tar.gz\n",
		"listed twice":       strings.Repeat("a", 64) + "  " + name + "\n" + strings.Repeat("b", 64) + "  " + name + "\n",
		"short hash":         "abc  " + name + "\n",
	} {
		s := newSite(t, "v1.0.0", func(s *site) { s.sums = sums })
		if _, err := s.source(t, &fakeVerifier{}).Gather(context.Background(), s.release(t, nil)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestGather_UnsupportedPlatformAndBadTag(t *testing.T) {
	s := newSite(t, "v1.0.0", nil)
	for _, p := range [][2]string{{"darwin", "arm64"}, {"windows", "amd64"}, {"linux", "riscv64"}, {"linux", "386"}} {
		src := s.source(t, nil)
		src.GOOS, src.GOARCH = p[0], p[1]
		if _, err := src.Gather(context.Background(), s.release(t, nil)); err == nil || !strings.Contains(err.Error(), "no signed release") {
			t.Errorf("%v: %v", p, err)
		}
	}
	if _, err := s.source(t, nil).Gather(context.Background(), update.Release{Tag: "latest"}); err == nil {
		t.Error("a non-version tag was accepted")
	}
}

func TestFetch_ChecksTheSHA256_BeforeAnythingCanBeInstalled(t *testing.T) {
	s := newSite(t, "v1.0.0", nil)
	src := s.source(t, &fakeVerifier{})
	ev, err := src.Gather(context.Background(), s.release(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := src.Fetch(context.Background(), &ev)
	if err != nil || ev.Computed != ev.ExpectedSHA || len(body) == 0 {
		t.Fatalf("err=%v computed=%s expected=%s", err, ev.Computed, ev.ExpectedSHA)
	}
	s.archive = append(s.archive, 'X') // the server now serves something else than what checksums.txt vouches for
	ev2, _ := src.Gather(context.Background(), s.release(t, nil))
	if _, err := src.Fetch(context.Background(), &ev2); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") || ev2.Computed == ev2.ExpectedSHA {
		t.Fatalf("a tampered archive must be refused: %v", err)
	}
}

// ---- cosign program ----

func TestCosignCLI_ExactIdentityIssuerAndFailureMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	shim := filepath.Join(dir, "cosign")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\nif [ -n \"$COSIGN_FAIL\" ]; then echo 'Error: none of the expected identities matched' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil { //nolint:gosec // a test shim must be executable
		t.Fatal(err)
	}
	c := CosignCLI{Path: shim}
	if !c.Available() {
		t.Fatal("shim not found")
	}
	if err := c.Verify(context.Background(), []byte("sums"), []byte("sig"), []byte("pem"), "ID", "ISS"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log) //nolint:gosec // temp file
	args := string(b)
	for _, want := range []string{"verify-blob", "--certificate\n", "--signature\n", "--certificate-identity\nID\n", "--certificate-oidc-issuer\nISS\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in\n%s", want, args)
		}
	}
	if strings.Contains(args, "regexp") {
		t.Errorf("a pattern flag would let another tag's signature through:\n%s", args)
	}
	t.Setenv("COSIGN_FAIL", "1")
	if err := c.Verify(context.Background(), nil, nil, nil, "ID", "ISS"); err == nil || !strings.Contains(err.Error(), "expected identities") {
		t.Fatalf("failure must carry cosign's reason: %v", err)
	}
	if (CosignCLI{Path: filepath.Join(dir, "missing")}).Available() {
		t.Fatal("a missing program is not available")
	}
	if err := (CosignCLI{Path: filepath.Join(dir, "missing")}).Verify(context.Background(), nil, nil, nil, "a", "b"); !errors.Is(err, ErrNoCosign) {
		t.Fatalf("err=%v", err)
	}
	_ = exec.ErrNotFound
}

func TestArchiveName_OnlyLinuxAmd64AndArm64(t *testing.T) {
	for _, tc := range []struct{ goos, arch string }{{"linux", "amd64"}, {"linux", "arm64"}} {
		if n, err := ArchiveName("1.0.0", tc.goos, tc.arch); err != nil || n != fmt.Sprintf("repo-keeper_1.0.0_linux_%s.tar.gz", tc.arch) {
			t.Errorf("%v: %q %v", tc, n, err)
		}
	}
}
