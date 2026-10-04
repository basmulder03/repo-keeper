// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"strings"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/provider"
	"github.com/basmulder03/repo-keeper/internal/provider/providertest"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

var urls = []string{
	"https://Git.Example.com/team/app.git",
	"git@git.example.com:team/sub/tools.git",
	"ssh://git@code.example.org:2222/solo.git",
	"https://git.example.com/team/web/",
}

func TestContract(t *testing.T) {
	want := func(full, clone string, ns ...string) provider.Repo {
		parts := strings.Split(full, "/")
		return provider.Repo{FullName: full, Namespace: ns, Name: parts[len(parts)-1], CloneURL: clone, SSHURL: clone, Private: true}
	}
	providertest.Run(t, providertest.Case{
		New: func(t *testing.T, _ string) provider.Provider {
			p, err := New(provider.Config{Remotes: urls})
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
		Kind: provider.Git, WantLogin: "", Static: true,
		WantRepos: []provider.Repo{
			want("git.example.com/team/app", "https://Git.Example.com/team/app.git", "git.example.com", "team"),
			want("git.example.com/team/sub/tools", "git@git.example.com:team/sub/tools.git", "git.example.com", "team", "sub"),
			want("code.example.org/solo", "ssh://git@code.example.org:2222/solo.git", "code.example.org"),
			want("git.example.com/team/web", "https://git.example.com/team/web/", "git.example.com", "team"),
		},
		MergedRepo: provider.Repo{FullName: "git.example.com/team/app"}, MergedBranches: []string{"x"},
	})
}

func TestNew_Refusals(t *testing.T) {
	tok := secrets.New("t")
	for name, tc := range map[string]struct {
		cfg  provider.Config
		want string
	}{
		"no urls":          {provider.Config{}, "urls is empty"},
		"base url":         {provider.Config{BaseURL: "https://x", Remotes: urls[:1]}, "base_url does not apply"},
		"password in url":  {provider.Config{Remotes: []string{"https://bob:hunter2@git.example.com/a/b.git"}}, "contains a password"},
		"cleartext http":   {provider.Config{Remotes: []string{"http://git.example.com/a/b.git"}}, "only https and ssh"},
		"file url":         {provider.Config{Remotes: []string{"file:///etc"}}, "only https and ssh"},
		"ext helper":       {provider.Config{Remotes: []string{"ext::sh-id"}}, "remote-helper"},
		"option lookalike": {provider.Config{Remotes: []string{"-oProxyCommand=x"}}, "unusable"},
		"local path":       {provider.Config{Remotes: []string{"/srv/git/repo.git"}}, "only https and ssh"},
		"no repo path":     {provider.Config{Remotes: []string{"https://git.example.com/"}}, "cannot tell"},
		"unsafe segment":   {provider.Config{Remotes: []string{"git@host:~user/repo.git"}}, "safe local path"},
		"traversal":        {provider.Config{Remotes: []string{"https://git.example.com/a/../../etc.git"}}, "safe local path"},
		"duplicate":        {provider.Config{Remotes: []string{"https://git.example.com/a/b.git", "https://git.example.com/a/b"}}, "same repository"},
		"token two hosts":  {provider.Config{Token: tok, Remotes: []string{"https://a.example.com/x/y.git", "https://b.example.com/x/y.git"}}, "span several hosts"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestNew_ErrorsNeverEchoPasswords(t *testing.T) {
	_, err := New(provider.Config{Remotes: []string{"https://bob:hunter2@git.example.com/a/b.git"}})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err=%v", err)
	}
}

func TestNew_TokenWithOneHttpsHostAndAnySshHosts_IsFine(t *testing.T) {
	_, err := New(provider.Config{Token: secrets.New("t"), Remotes: []string{
		"https://a.example.com/x/y.git", "https://a.example.com/x/z.git", "git@b.example.com:x/y.git", "git@c.example.com:x/y.git",
	}})
	if err != nil {
		t.Fatalf("a token is only ever offered to https hosts, and there is one: %v", err)
	}
}

func TestParse_StripsUserFromHTTPSOnly(t *testing.T) {
	r, https, err := Parse("https://bob@git.example.com/a/b.git")
	if err != nil || !https || r.CloneURL != "https://git.example.com/a/b.git" {
		t.Fatalf("r=%+v https=%v err=%v", r, https, err)
	}
	r, https, err = Parse("ssh://git@git.example.com/a/b.git")
	if err != nil || https || r.SSHURL != "ssh://git@git.example.com/a/b.git" {
		t.Fatalf("ssh needs its user: r=%+v https=%v err=%v", r, https, err)
	}
}

func TestMergedBranches_IsEmptyNeverAnError(t *testing.T) {
	p, _ := New(provider.Config{Remotes: urls[:1]})
	got, err := p.MergedBranches(t.Context(), provider.Repo{}, []string{"a", "b"})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got=%v err=%v", got, err)
	}
}
