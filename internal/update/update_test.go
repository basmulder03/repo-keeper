// SPDX-License-Identifier: Apache-2.0

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
)

func mustVer(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return v
}

func TestVersion_OrderingFollowsSemVer(t *testing.T) {
	ordered := []string{ // strictly increasing, straight from the SemVer 2.0 examples plus our beta numbering
		"0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta", "0.1.0-beta.2", "0.1.0-beta.9", "0.1.0-beta.10", "0.1.0-rc.1", "0.1.0", "0.1.1", "0.2.0", "1.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := mustVer(t, ordered[i]).Compare(mustVer(t, ordered[j])); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestParseVersion_AcceptsReleasesRejectsEverythingElse(t *testing.T) {
	for in, want := range map[string]string{"1.2.3": "1.2.3", "v1.2.3-beta.4": "1.2.3-beta.4", " 0.1.0+build.5 ": "0.1.0"} {
		if got := mustVer(t, in).String(); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "dev", "0166000-dirty", "1.2", "01.2.3", "1.2.3-", "1.2.3-beta..1", "v", "1.2.3.4", "latest"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func rel(tag string, pre, draft bool) Release {
	return Release{Tag: tag, Prerelease: pre, Draft: draft, URL: "https://x/" + tag}
}

func TestNewest_StableInstallIsNeverNudgedOntoABeta(t *testing.T) {
	rs := []Release{rel("v0.2.0-beta.1", true, false), rel("v0.1.0", false, false), rel("v0.1.1", false, true), rel("junk", false, false)}
	r, v, err := Newest(rs, mustVer(t, "0.0.9"))
	if err != nil || r.Tag != "v0.1.0" || v.String() != "0.1.0" {
		t.Fatalf("stable user: %v %v %v", r.Tag, v, err)
	}
	r, _, err = Newest(rs, mustVer(t, "0.1.0-beta.5"))
	if err != nil || r.Tag != "v0.2.0-beta.1" {
		t.Fatalf("beta user must see betas: %v %v", r.Tag, err)
	}
	if _, _, err := Newest([]Release{rel("v1.0.0-rc.1", true, false)}, mustVer(t, "0.9.0")); err == nil {
		t.Fatal("only a pre-release exists: a stable user has nothing to update to")
	}
}

func TestFind_ExactVersionOnly_NeverDrafts(t *testing.T) {
	rs := []Release{rel("v1.0.0", false, true), rel("v0.9.0", false, false)}
	if _, ok := Find(rs, mustVer(t, "1.0.0")); ok {
		t.Fatal("a draft must not be found")
	}
	if r, ok := Find(rs, mustVer(t, "v0.9.0")); !ok || r.Tag != "v0.9.0" {
		t.Fatal("exact published version not found")
	}
}

func TestRelease_Urgent_ByConvention(t *testing.T) {
	for body, want := range map[string]bool{
		"### Security\n- fixed a token leak":        true,
		"## security fixes\nstuff":                  true,
		"- **Security**: patched X":                 true,
		"- security: patched X":                     true,
		"This fixes CVE-2026-12345 in a library.":   true,
		"### Added\n- a feature about security UX":  false,
		"Improved security of the logs, no change.": false,
		"": false,
	} {
		if got := (Release{Body: body}).Urgent(); got != want {
			t.Errorf("%q: urgent=%v, want %v", body, got, want)
		}
	}
}

func TestDetect_Channels(t *testing.T) {
	exists := func(paths ...string) Probe {
		return Probe{Exists: func(p string) bool {
			for _, x := range paths {
				if x == p {
					return true
				}
			}
			return false
		}}
	}
	for name, tc := range map[string]struct {
		exe   string
		probe Probe
		want  Kind
		self  bool
		cmd   string
	}{
		"nix":        {"/nix/store/abc-repo-keeper-0.1.0/bin/repo-keeper", exists(), Nix, false, "nix profile upgrade"},
		"container":  {"/home/u/.local/bin/repo-keeper", exists("/.dockerenv"), Container, false, "newer image"},
		"podman":     {"/usr/local/bin/repo-keeper", exists("/run/.containerenv"), Container, false, "newer image"},
		"homebrew":   {"/opt/homebrew/Cellar/repo-keeper/0.1.0/bin/repo-keeper", exists(), Homebrew, false, "brew upgrade"},
		"deb":        {"/usr/bin/repo-keeper", exists("/etc/debian_version"), Package, false, "apt install"},
		"rpm":        {"/usr/bin/repo-keeper", exists("/etc/fedora-release"), Package, false, "dnf install"},
		"other pkg":  {"/usr/bin/repo-keeper", exists(), Package, false, "package manager"},
		"tarball":    {"/home/u/.local/bin/repo-keeper", exists(), Tarball, true, ""},
		"usr local":  {"/usr/local/bin/repo-keeper", exists(), Tarball, true, ""},
		"opt custom": {"/opt/tools/repo-keeper", exists(), Tarball, true, ""},
	} {
		c := Detect(tc.exe, tc.probe)
		if c.Kind != tc.want || c.SelfUpdatable() != tc.self || !strings.Contains(c.Command, tc.cmd) {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestDetect_FollowsSymlinksIntoTheNixStore(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "repo-keeper")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	want, err := filepath.EvalSymlinks(target) // the temp dir itself may sit behind a symlink (macOS: /var -> /private/var)
	if err != nil {
		t.Fatal(err)
	}
	if c := Detect(link, Probe{Exists: func(string) bool { return false }}); c.Exe != want || c.Kind != Tarball {
		t.Fatalf("%+v want exe %s", c, want)
	}
}

func TestState_RoundTrip_PrivateFile_AvailableOnlyWhenNewer(t *testing.T) {
	dir := t.TempDir()
	if s := ReadState(dir); !s.Checked.IsZero() || s.Available("0.1.0") {
		t.Fatalf("missing file must be an empty state: %+v", s)
	}
	in := State{Checked: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), Latest: "0.1.0-beta.6", URL: "https://x/6", Urgent: true}
	if err := WriteState(dir, in); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, StateFile)); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) { // no POSIX mode bits on Windows
		t.Fatalf("state file mode: %v %v", fi, err)
	}
	out := ReadState(dir)
	if !out.Checked.Equal(in.Checked) || out.Latest != in.Latest || !out.Urgent {
		t.Fatalf("out=%+v", out)
	}
	for cur, want := range map[string]bool{"0.1.0-beta.5": true, "0.1.0-beta.6": false, "0.1.0-beta.7": false, "0.1.0": false, "dev": false, "0166000-dirty": false} {
		if got := out.Available(cur); got != want {
			t.Errorf("Available(%q)=%v, want %v", cur, got, want)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := ReadState(dir); !s.Checked.IsZero() {
		t.Fatal("a corrupt state file must read as empty, never fail the daemon")
	}
}

func newHTTP(t *testing.T) *httpx.Client {
	t.Helper()
	c, err := httpx.New(httpx.Config{UserAgent: "repo-keeper/test", Limiter: ratelimit.New(ratelimit.Config{Rate: 1000, Burst: 1000}, clock.Real{}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChecker_Releases_OneGET_StandardUserAgent_NoDrafts(t *testing.T) {
	var gets int
	var gotUA, gotAccept, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		gotUA, gotAccept, gotQuery, gotAuth = r.UserAgent(), r.Header.Get("Accept"), r.URL.RawQuery, r.Header.Get("Authorization")
		if r.URL.Path != "/repos/o/r/releases" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"tag_name":"v2.0.0","draft":true},{"tag_name":"v1.0.0","draft":false,"prerelease":false,"html_url":"https://x","published_at":"2026-10-01T00:00:00Z","assets":[{"name":"a","browser_download_url":"https://x/a","size":5}]}]`))
	}))
	defer srv.Close()
	rs, err := Checker{HTTP: newHTTP(t), API: srv.URL + "/", Repo: "o/r"}.Releases(context.Background())
	if err != nil || len(rs) != 1 || rs[0].Tag != "v1.0.0" {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	if a, ok := rs[0].Asset("a"); !ok || a.URL != "https://x/a" {
		t.Fatalf("asset lookup: %+v", rs[0])
	}
	if gets != 1 || gotUA != "repo-keeper/test" || gotAccept != "application/vnd.github+json" || gotQuery != "per_page=30" || gotAuth != "" {
		t.Fatalf("gets=%d ua=%q accept=%q query=%q auth=%q: the check must be one anonymous GET carrying only the standard User-Agent", gets, gotUA, gotAccept, gotQuery, gotAuth)
	}
}

func TestChecker_Releases_ErrorsAreErrors(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"http 403": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "rate limited", http.StatusForbidden) },
		"not json": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
	} {
		srv := httptest.NewServer(h)
		_, err := Checker{HTTP: newHTTP(t), API: srv.URL}.Releases(context.Background())
		srv.Close()
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
