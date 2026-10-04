// SPDX-License-Identifier: Apache-2.0

// Package providertest is the behaviour every provider must satisfy; each provider's tests run it against a
// fake server that serves the same logical fixture.
package providertest

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/provider"
)

// Case describes one provider and the fixture its fake server serves.
type Case struct {
	// New returns a provider talking to the fake server with the given token. The server must answer 401 for
	// every token other than GoodToken.
	New       func(t *testing.T, token string) provider.Provider
	GoodToken string
	Kind      provider.Kind
	// WantLogin is what CheckAuth must report.
	WantLogin string
	// WantRepos is the complete expected listing (any order). The fixture must span several pages and include
	// a nested namespace if the platform supports one.
	WantRepos []provider.Repo
	// Merged: the repo to query, branches to ask about, and the exact expected answer. The fixture must also
	// contain a merged PR from a fork and an unmerged PR for one of the asked branches (both must be ignored).
	MergedRepo     provider.Repo
	MergedBranches []string
	WantMerged     map[string]string
}

// asDaemonCalls keeps only what the daemon passes to MergedBranches (FullName); providers must not rely on anything else.
func asDaemonCalls(r provider.Repo) provider.Repo { return provider.Repo{FullName: r.FullName} }

// Run executes the contract.
func Run(t *testing.T, c Case) {
	t.Helper()
	ctx := context.Background()

	t.Run("identity", func(t *testing.T) {
		p := c.New(t, c.GoodToken)
		if p.Kind() != c.Kind || p.APIHost() == "" || p.GitUsername() == "" {
			t.Fatalf("kind=%q host=%q user=%q", p.Kind(), p.APIHost(), p.GitUsername())
		}
	})

	t.Run("check auth", func(t *testing.T) {
		a, err := c.New(t, c.GoodToken).CheckAuth(ctx)
		if err != nil || a.Login != c.WantLogin {
			t.Fatalf("auth=%+v err=%v", a, err)
		}
	})

	t.Run("bad token is ErrAuth and never echoed", func(t *testing.T) {
		const bad = "CANARY-bad-token-value"
		p := c.New(t, bad)
		for name, call := range map[string]func() error{
			"CheckAuth": func() error { _, err := p.CheckAuth(ctx); return err },
			"ListRepos": func() error { _, err := p.ListRepos(ctx); return err },
			"Merged": func() error {
				_, err := p.MergedBranches(ctx, asDaemonCalls(c.MergedRepo), c.MergedBranches)
				return err
			},
		} {
			err := call()
			if !errors.Is(err, provider.ErrAuth) {
				t.Errorf("%s: err=%v, want ErrAuth", name, err)
			} else if strings.Contains(err.Error(), bad) {
				t.Errorf("%s: error leaks the token: %v", name, err)
			}
		}
	})

	t.Run("list repos is complete and correctly mapped", func(t *testing.T) {
		got, err := c.New(t, c.GoodToken).ListRepos(ctx)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]provider.Repo{}
		for _, r := range got {
			if _, dup := byName[r.FullName]; dup {
				t.Errorf("duplicate %s", r.FullName)
			}
			byName[r.FullName] = r
		}
		if len(byName) != len(c.WantRepos) {
			names := make([]string, 0, len(byName))
			for n := range byName {
				names = append(names, n)
			}
			sort.Strings(names)
			t.Fatalf("got %d repos %v, want %d", len(byName), names, len(c.WantRepos))
		}
		for _, w := range c.WantRepos {
			g, ok := byName[w.FullName]
			if !ok {
				t.Errorf("missing %s", w.FullName)
				continue
			}
			if g.Name != w.Name || strings.Join(g.Namespace, "/") != strings.Join(w.Namespace, "/") || g.DefaultBranch != w.DefaultBranch ||
				g.CloneURL != w.CloneURL || g.SSHURL != w.SSHURL || g.Private != w.Private || g.Archived != w.Archived || g.Fork != w.Fork || g.Disabled != w.Disabled || g.ID == "" {
				t.Errorf("%s:\n got  %+v\n want %+v", w.FullName, g, w)
			}
			if g.FullName != strings.Join(append(append([]string{}, g.Namespace...), g.Name), "/") {
				t.Errorf("%s: FullName disagrees with Namespace/Name: %+v", w.FullName, g)
			}
			// every repo must be mappable to a safe local path
			if _, err := provider.LocalPath("/root", c.Kind, g); err != nil {
				t.Errorf("%s: %v", w.FullName, err)
			}
		}
	})

	t.Run("merged branches ignore forks and unmerged PRs", func(t *testing.T) {
		got, err := c.New(t, c.GoodToken).MergedBranches(ctx, asDaemonCalls(c.MergedRepo), c.MergedBranches)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(c.WantMerged) {
			t.Fatalf("got %v, want %v", got, c.WantMerged)
		}
		for k, v := range c.WantMerged {
			if got[k] != v {
				t.Errorf("%s: got %q want %q", k, got[k], v)
			}
		}
	})

	t.Run("cancelled context fails fast", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		done := make(chan error, 1)
		go func() { _, err := c.New(t, c.GoodToken).ListRepos(cctx); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected an error")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled call did not return")
		}
	})
}
