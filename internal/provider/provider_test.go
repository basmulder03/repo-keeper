// SPDX-License-Identifier: Apache-2.0

package provider

import (
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
