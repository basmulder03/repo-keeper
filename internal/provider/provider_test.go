// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repo(full string) Repo {
	parts := strings.Split(full, "/")
	return Repo{FullName: full, Namespace: parts[:len(parts)-1], Name: parts[len(parts)-1]}
}

func TestMatches(t *testing.T) {
	tests := []struct {
		name     string
		inc, exc []string
		full     string
		want     bool
	}{
		{"no rules", nil, nil, "a/b", true},
		{"include org", []string{"acme/*"}, nil, "acme/api", true},
		{"include other org", []string{"acme/*"}, nil, "other/api", false},
		{"case insensitive", []string{"ACME/*"}, nil, "acme/Api", true},
		{"exclude wins", []string{"acme/*"}, []string{"acme/archive-*"}, "acme/archive-2019", false},
		{"star does not cross slash", []string{"*"}, nil, "acme/api", false},
		{"double star crosses levels", []string{"acme/**"}, nil, "acme/platform/infra/tf", true},
		{"single star one level only", []string{"acme/*"}, nil, "acme/platform/infra/tf", false},
		{"middle double star", []string{"acme/**/tf"}, nil, "acme/platform/infra/tf", true},
		{"question mark", []string{"acme/ap?"}, nil, "acme/api", true},
		{"question mark not slash", []string{"acme?api"}, nil, "acme/api", false},
		{"literal dots are literal", []string{"a.b/c"}, nil, "axb/c", false},
		{"exclude nested", []string{"**"}, []string{"**/archive*"}, "g/sub/archive-old", false},
		{"exclude only", nil, []string{"*/secret"}, "acme/secret", false},
		{"bad glob matches nothing", []string{"["}, nil, "a/b", false},
		{"empty glob matches nothing", []string{""}, nil, "a/b", false},
	}
	for _, tc := range tests {
		if got := Matches(tc.inc, tc.exc, repo(tc.full)); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestLocalPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	got, err := LocalPath(root, GitHub, repo("acme/api"))
	if err != nil || got != filepath.Join(root, "github", "acme", "api") {
		t.Fatalf("got=%q err=%v", got, err)
	}
	deep, err := LocalPath(root, "gitlab", repo("g/sub/leaf"))
	if err != nil || deep != filepath.Join(root, "gitlab", "g", "sub", "leaf") {
		t.Fatalf("deep=%q err=%v", deep, err)
	}
	for _, bad := range []string{"../evil", "a/..", "a/.", "a/-rf", "a/na me", "a/x.", "./a/b", "a/b\x00"} {
		if _, err := LocalPath(root, GitHub, repo(bad)); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestNew_UnknownKind(t *testing.T) {
	if _, err := New("nope", Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidGlob(t *testing.T) {
	for _, ok := range []string{"a/b", "acme/*", "acme/**", "a?c/**/x", "*/archive-*"} {
		if err := ValidGlob(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "[abc]/x", "a\\b", "a]"} {
		if ValidGlob(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestValidCloneURL(t *testing.T) {
	for _, ok := range []string{"https://github.com/o/r.git", "ssh://git@host:2222/o/r.git", "git@github.com:o/r.git", "https://gitlab.example.com/a/b/c.git"} {
		if err := ValidCloneURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "-oProxyCommand=evil", "http://example.com/r.git", "git://example.com/r.git", "file:///etc/passwd", "/srv/git/r.git", "../r",
		"ext::sh -c touch% /tmp/x", "https://", "https://host/a b", "ftp://h/r", "C:\\repos\\r", "https://user:pw@host/r\n", "a::b",
	} {
		if err := ValidCloneURL(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		} else if strings.Contains(err.Error(), "pw@") {
			t.Errorf("error leaks credentials: %v", err)
		}
	}
}

func FuzzValidCloneURL_NeverPanics(f *testing.F) {
	for _, s := range []string{"https://a/b", "git@h:p", "::", "-x", "ssh://[::1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidCloneURL(s) == nil && (strings.HasPrefix(s, "-") || strings.Contains(s, "::")) {
			t.Fatalf("accepted dangerous URL %q", s)
		}
	})
}

func TestCheckNoSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "github", "acme"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CheckNoSymlinks(root, filepath.Join(root, "github", "acme", "new")); err != nil {
		t.Fatalf("plain tree rejected: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "github", "evil")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := CheckNoSymlinks(root, filepath.Join(root, "github", "evil", "repo")); err == nil {
		t.Fatal("clone through a symlinked namespace directory must be refused")
	}
	if err := CheckNoSymlinks(root, filepath.Join(outside, "x")); err == nil {
		t.Fatal("destination outside the root must be refused")
	}
	rootLink := filepath.Join(t.TempDir(), "link")
	_ = os.Symlink(root, rootLink)
	if err := CheckNoSymlinks(rootLink, filepath.Join(rootLink, "github", "acme", "ok")); err != nil {
		t.Fatalf("a symlinked root chosen by the user is allowed: %v", err)
	}
}

func FuzzLocalPath_NeverEscapesRoot(f *testing.F) {
	for _, s := range []string{"a/b", "../x", "a/../../b", "a/./b", "a/b/..", "a//b", "A/b/c/d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, full string) {
		parts := strings.Split(full, "/")
		r := Repo{FullName: full, Namespace: parts[:len(parts)-1], Name: parts[len(parts)-1]}
		root := filepath.Join(os.TempDir(), "rk-fuzz-root")
		p, err := LocalPath(root, GitHub, r)
		if err != nil {
			return
		}
		if rel, rerr := filepath.Rel(root, p); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("%q escaped the root: %s", full, p)
		}
	})
}

func FuzzMatches_NeverPanics(f *testing.F) {
	for _, s := range []string{"a/**", "*", "?", "**/x", "[", "a.b", "\\", "(((", "**/**/**"} {
		f.Add(s, "acme/platform/x")
	}
	f.Fuzz(func(t *testing.T, glob, name string) {
		_ = Matches([]string{glob}, []string{glob}, Repo{FullName: name})
		_ = Matches([]string{glob}, nil, Repo{FullName: name})
	})
}
