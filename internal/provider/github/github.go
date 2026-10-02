// SPDX-License-Identifier: Apache-2.0

// Package github implements provider.Provider for github.com and GitHub Enterprise Server.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/provider"
)

const (
	defaultAPI = "https://api.github.com"
	maxPages   = 200
	prPages    = 3
)

func init() { provider.Register(provider.GitHub, New) }

type gh struct {
	base *url.URL
	cfg  provider.Config
}

// New builds a GitHub provider; BaseURL defaults to https://api.github.com (GHES: https://host/api/v3).
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.HTTP == nil {
		return nil, errors.New("github: HTTP client required")
	}
	b := cfg.BaseURL
	if b == "" {
		b = defaultAPI
	}
	u, err := url.Parse(strings.TrimRight(b, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("github: invalid base_url %q", b)
	}
	return &gh{base: u, cfg: cfg}, nil
}

func (g *gh) Kind() provider.Kind { return provider.GitHub }
func (g *gh) APIHost() string     { return g.base.Host }
func (g *gh) GitUsername() string { return "x-access-token" }

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *APIError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Message) }

// get performs an authenticated GET; absolute URLs must stay on the API host so the token never leaves it.
func (g *gh) get(ctx context.Context, target string) (*httpx.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if !u.IsAbs() {
		u = g.base.JoinPath(u.Path)
		u.RawQuery = mustQuery(target)
	}
	if u.Host != g.base.Host || u.Scheme != g.base.Scheme {
		return nil, fmt.Errorf("github: refusing to send credentials to %s", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token.Reveal())
	resp, err := g.cfg.HTTP.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.Status == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", provider.ErrAuth, message(resp.Body))
	case resp.Status < 200 || resp.Status > 299:
		return nil, &APIError{Status: resp.Status, Message: message(resp.Body)}
	}
	return resp, nil
}

func mustQuery(target string) string {
	if _, q, ok := strings.Cut(target, "?"); ok {
		return q
	}
	return ""
}

func message(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return m.Message
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return strings.TrimSpace(string(body))
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func next(h http.Header) string {
	if m := nextLink.FindStringSubmatch(h.Get("Link")); m != nil {
		return m[1]
	}
	return ""
}

func (g *gh) CheckAuth(ctx context.Context) (provider.Auth, error) {
	resp, err := g.get(ctx, "/user")
	if err != nil {
		return provider.Auth{}, err
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(resp.Body, &u); err != nil {
		return provider.Auth{}, fmt.Errorf("github: decoding /user: %w", err)
	}
	a := provider.Auth{Login: u.Login}
	if s := resp.Header.Get("X-OAuth-Scopes"); s != "" {
		for _, sc := range strings.Split(s, ",") {
			a.Scopes = append(a.Scopes, strings.TrimSpace(sc))
		}
	}
	if e := resp.Header.Get("github-authentication-token-expiration"); e != "" {
		if t, err := time.Parse("2006-01-02 15:04:05 MST", e); err == nil {
			a.Expires = t
		}
	}
	a.Warnings = warnings(a)
	return a, nil
}

func warnings(a provider.Auth) []string {
	var w []string
	for _, s := range a.Scopes {
		switch s {
		case "repo", "delete_repo", "workflow", "admin:org", "admin:repo_hook", "write:packages", "admin:public_key":
			w = append(w, fmt.Sprintf("token has the broad %q scope; repo-keeper only reads. Prefer a fine-grained token with Contents:read, Metadata:read, Pull requests:read", s))
		}
	}
	if !a.Expires.IsZero() && time.Until(a.Expires) < 14*24*time.Hour {
		w = append(w, "token expires "+a.Expires.Format("2006-01-02"))
	}
	return w
}

type repoJSON struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Disabled      bool   `json:"disabled"`
}

func (g *gh) ListRepos(ctx context.Context) ([]provider.Repo, error) {
	var out []provider.Repo
	target := "/user/repos?per_page=100&sort=full_name&affiliation=owner,collaborator,organization_member"
	for page := 0; target != ""; page++ {
		if page >= maxPages {
			return nil, errors.New("github: too many pages listing repositories")
		}
		resp, err := g.get(ctx, target)
		if err != nil {
			return nil, err
		}
		var rs []repoJSON
		if err := json.Unmarshal(resp.Body, &rs); err != nil {
			return nil, fmt.Errorf("github: decoding repositories: %w", err)
		}
		for _, r := range rs {
			ns, name, ok := strings.Cut(r.FullName, "/")
			if !ok || name == "" {
				continue
			}
			out = append(out, provider.Repo{
				ID: strconv.FormatInt(r.ID, 10), FullName: r.FullName, Namespace: []string{ns}, Name: name,
				CloneURL: r.CloneURL, SSHURL: r.SSHURL, DefaultBranch: r.DefaultBranch,
				Private: r.Private, Archived: r.Archived, Fork: r.Fork, Disabled: r.Disabled,
			})
		}
		target = next(resp.Header)
	}
	return out, nil
}

func (g *gh) MergedBranches(ctx context.Context, repo provider.Repo, branches []string) (map[string]string, error) {
	want := map[string]bool{}
	for _, b := range branches {
		want[b] = true
	}
	found := map[string]string{}
	target := "/repos/" + repo.FullName + "/pulls?state=closed&sort=updated&direction=desc&per_page=100"
	for page := 0; target != "" && page < prPages && len(found) < len(want); page++ {
		resp, err := g.get(ctx, target)
		if err != nil {
			return nil, err
		}
		var prs []struct {
			MergedAt *time.Time `json:"merged_at"`
			Head     struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		}
		if err := json.Unmarshal(resp.Body, &prs); err != nil {
			return nil, fmt.Errorf("github: decoding pull requests: %w", err)
		}
		for _, p := range prs {
			// only PRs from this repository's own branches; a fork's same-named branch proves nothing
			if p.MergedAt == nil || p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, repo.FullName) {
				continue
			}
			if _, done := found[p.Head.Ref]; want[p.Head.Ref] && !done {
				found[p.Head.Ref] = p.Head.SHA
			}
		}
		target = next(resp.Header)
	}
	return found, nil
}
