// SPDX-License-Identifier: Apache-2.0

// Package gitea implements provider.Provider for Gitea and Forgejo (including Codeberg), REST API v1.
package gitea

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

	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/provider"
)

const (
	maxPages = 500
	prPages  = 3
	pageSize = 50 // the default server-side maximum; asking for more is silently capped
)

func init() {
	provider.Register(provider.Gitea, factory(provider.Gitea))
	provider.Register(provider.Forgejo, factory(provider.Forgejo))
}

type gt struct {
	kind provider.Kind
	base *url.URL
	cfg  provider.Config
}

func factory(k provider.Kind) provider.Factory {
	return func(cfg provider.Config) (provider.Provider, error) { return New(k, cfg) }
}

// New builds a Gitea/Forgejo provider. BaseURL is required (there is no single public host): the instance root
// (https://git.example.com) or the full API URL; "/api/v1" is appended when missing.
func New(k provider.Kind, cfg provider.Config) (provider.Provider, error) {
	if cfg.HTTP == nil {
		return nil, errors.New("gitea: HTTP client required")
	}
	b := strings.TrimRight(cfg.BaseURL, "/")
	if b == "" {
		return nil, fmt.Errorf("%s: base_url is required (the address of your server, e.g. https://codeberg.org)", k)
	}
	u, err := url.Parse(b)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s: invalid base_url %q", k, cfg.BaseURL)
	}
	if !strings.HasSuffix(u.Path, "/api/v1") {
		u.Path = strings.TrimRight(u.Path, "/") + "/api/v1"
	}
	u.RawPath = ""
	return &gt{kind: k, base: u, cfg: cfg}, nil
}

func (g *gt) Kind() provider.Kind { return g.kind }
func (g *gt) APIHost() string     { return g.base.Host }

// GitUsername implements provider.Provider; Gitea takes the token as the password with any non-empty user.
func (g *gt) GitUsername() string { return "oauth2" }

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *APIError) Error() string { return fmt.Sprintf("gitea: HTTP %d: %s", e.Status, e.Message) }

// endpoint builds an API URL; each segment is escaped as one path element.
func (g *gt) endpoint(query string, segments ...string) string {
	u := *g.base
	decoded, raw := u.Path, u.Path
	for _, s := range segments {
		decoded += "/" + s
		raw += "/" + url.PathEscape(s)
	}
	u.Path, u.RawPath, u.RawQuery = decoded, raw, query
	return u.String()
}

// get performs an authenticated GET; absolute URLs must stay on the API host so the token never leaves it.
func (g *gt) get(ctx context.Context, target string) (*httpx.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if u.Host != g.base.Host || u.Scheme != g.base.Scheme {
		return nil, fmt.Errorf("gitea: refusing to send credentials to %s", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+g.cfg.Token.Reveal())
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

// CheckAuth implements provider.Provider; Gitea exposes no token scopes or expiry to the token itself, so only the login is known.
func (g *gt) CheckAuth(ctx context.Context) (provider.Auth, error) {
	resp, err := g.get(ctx, g.endpoint("", "user"))
	if err != nil {
		return provider.Auth{}, err
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(resp.Body, &u); err != nil || u.Login == "" {
		return provider.Auth{}, fmt.Errorf("gitea: decoding /user: %w", errors.Join(err, errors.New("no login in answer")))
	}
	return provider.Auth{Login: u.Login, Warnings: []string{"scopes and expiry cannot be read from a Gitea/Forgejo token; create it with only read:repository (and read:user)"}}, nil
}

type repoJSON struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Empty         bool   `json:"empty"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// ListRepos implements provider.Provider.
func (g *gt) ListRepos(ctx context.Context) ([]provider.Repo, error) {
	var out []provider.Repo
	target := g.endpoint("limit="+strconv.Itoa(pageSize), "user", "repos")
	for page := 0; target != ""; page++ {
		if page >= maxPages {
			return nil, errors.New("gitea: too many pages listing repositories")
		}
		resp, err := g.get(ctx, target)
		if err != nil {
			return nil, err
		}
		var rs []repoJSON
		if err := json.Unmarshal(resp.Body, &rs); err != nil {
			return nil, fmt.Errorf("gitea: decoding repositories: %w", err)
		}
		for _, r := range rs {
			if r.Owner.Login == "" || r.Name == "" {
				continue
			}
			out = append(out, provider.Repo{
				ID: strconv.FormatInt(r.ID, 10), FullName: r.Owner.Login + "/" + r.Name,
				Namespace: []string{r.Owner.Login}, Name: r.Name,
				CloneURL: r.CloneURL, SSHURL: r.SSHURL, DefaultBranch: r.DefaultBranch,
				Private: r.Private, Archived: r.Archived, Fork: r.Fork,
				Disabled: r.Empty, // nothing to sync yet
			})
		}
		target = next(resp.Header)
	}
	return out, nil
}

// MergedBranches implements provider.Provider.
func (g *gt) MergedBranches(ctx context.Context, repo provider.Repo, branches []string) (map[string]string, error) {
	if len(repo.Namespace) != 1 {
		return nil, fmt.Errorf("gitea: unexpected namespace in %q", repo.FullName)
	}
	want := map[string]bool{}
	for _, b := range branches {
		want[b] = true
	}
	found := map[string]string{}
	target := g.endpoint("state=closed&sort=recentupdate&limit="+strconv.Itoa(pageSize), "repos", repo.Namespace[0], repo.Name, "pulls")
	for page := 0; target != "" && page < prPages && len(found) < len(want); page++ {
		resp, err := g.get(ctx, target)
		if err != nil {
			return nil, err
		}
		var prs []struct {
			Merged bool `json:"merged"`
			Head   struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Repo *struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"base"`
		}
		if err := json.Unmarshal(resp.Body, &prs); err != nil {
			return nil, fmt.Errorf("gitea: decoding pull requests: %w", err)
		}
		for _, p := range prs {
			// only PRs from this repository's own branches; a fork's (or a deleted fork's) same-named branch proves nothing
			if !p.Merged || p.Head.SHA == "" || p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.ID != p.Base.Repo.ID {
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
