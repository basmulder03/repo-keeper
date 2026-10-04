// SPDX-License-Identifier: Apache-2.0

// Package generic implements provider.Provider for plain git remotes: the user lists the clone URLs, there is no
// platform API, no login and no pull-request data.
package generic

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/basmulder03/repo-keeper/internal/provider"
)

func init() { provider.Register(provider.Git, New) }

type static struct{ repos []provider.Repo }

// New builds the provider from Config.Remotes. A credential is optional (SSH keys and public repositories need none);
// when one is given it is only ever offered to a single HTTPS host, so a token for one server can never reach another.
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.BaseURL != "" {
		return nil, errors.New("generic git: base_url does not apply (the repositories are the urls list)")
	}
	if len(cfg.Remotes) == 0 {
		return nil, errors.New("generic git: urls is empty; list at least one clone URL")
	}
	s := &static{}
	seen := map[string]string{}
	httpsHosts := map[string]bool{}
	for _, raw := range cfg.Remotes {
		r, https, err := Parse(raw)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[r.FullName]; dup {
			return nil, fmt.Errorf("generic git: %q and %q are the same repository (%s)", prev, shown(raw), r.FullName)
		}
		seen[r.FullName] = shown(raw)
		if https {
			httpsHosts[r.Namespace[0]] = true
		}
		s.repos = append(s.repos, r)
	}
	if !cfg.Token.IsZero() && len(httpsHosts) > 1 {
		return nil, errors.New("generic git: a credential is set but the https URLs span several hosts; use one account per host so a token is never sent to the wrong server")
	}
	return s, nil
}

func (*static) Kind() provider.Kind { return provider.Git }
func (*static) APIHost() string     { return "static" }

// GitUsername implements provider.Provider; the user name is ignored by most servers when a token is the password.
func (*static) GitUsername() string { return "git" }

// CheckAuth implements provider.Provider; there is nothing to authenticate against, so there is no login either.
func (*static) CheckAuth(context.Context) (provider.Auth, error) { return provider.Auth{}, nil }

// ListRepos implements provider.Provider.
func (s *static) ListRepos(ctx context.Context) ([]provider.Repo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]provider.Repo(nil), s.repos...), nil
}

// MergedBranches implements provider.Provider; a plain remote has no pull requests, so cleanup relies on git alone.
func (*static) MergedBranches(context.Context, provider.Repo, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

// Parse turns one clone URL into a Repo named "host/path" (so clones of different servers never collide) and
// reports whether the URL is https. https and ssh (URL or scp-like) forms are accepted; credentials in the URL are not.
func Parse(raw string) (repo provider.Repo, https bool, err error) {
	raw = strings.TrimSpace(raw)
	if err := provider.ValidCloneURL(raw); err != nil {
		return repo, false, err
	}
	var host, path, clone string
	switch {
	case strings.Contains(raw, "://"):
		u, perr := url.Parse(raw)
		if perr != nil {
			return repo, false, fmt.Errorf("generic git: unusable URL %q", shown(raw))
		}
		if _, hasPass := u.User.Password(); hasPass {
			return repo, false, fmt.Errorf("generic git: %q contains a password; credentials never belong in the URL (use a token, or SSH keys)", shown(raw))
		}
		host, path, https = strings.ToLower(u.Hostname()), u.Path, u.Scheme == "https"
		if https {
			u.User = nil // a user name adds nothing for https; the token comes from the secret store
		}
		clone = u.String()
	default: // scp-like user@host:path
		at, colon := strings.Index(raw, "@"), strings.Index(raw, ":")
		host, path, clone = strings.ToLower(raw[at+1:colon]), raw[colon+1:], raw
	}
	segs := strings.FieldsFunc(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), func(r rune) bool { return r == '/' })
	if host == "" || len(segs) == 0 {
		return repo, false, fmt.Errorf("generic git: cannot tell host and repository from %q", shown(raw))
	}
	ns := append([]string{host}, segs[:len(segs)-1]...)
	name := segs[len(segs)-1]
	repo = provider.Repo{
		ID: "git:" + host + "/" + strings.Join(segs, "/"), FullName: host + "/" + strings.Join(segs, "/"),
		Namespace: ns, Name: name,
		CloneURL: clone, SSHURL: clone, // the listed URL is the one to use whatever clone_protocol says
		Private: true, // unknown; treat as private
	}
	if _, perr := provider.LocalPath("/root", provider.Git, repo); perr != nil {
		return repo, false, fmt.Errorf("generic git: %q cannot be cloned to a safe local path: %w", shown(raw), perr)
	}
	return repo, https, nil
}

// shown keeps user info out of messages.
func shown(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		return u.Redacted()
	}
	return raw
}
