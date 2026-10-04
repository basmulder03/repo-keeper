// SPDX-License-Identifier: Apache-2.0

// Package bitbucket implements provider.Provider for Bitbucket Cloud (REST API 2.0).
package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/provider"
)

const (
	defaultAPI = "https://api.bitbucket.org/2.0"
	maxPages   = 500
	prPages    = 3
	pageLen    = 100 // documented global maximum
)

func init() { provider.Register(provider.Bitbucket, New) }

type bb struct {
	base *url.URL
	cfg  provider.Config
}

// New builds a Bitbucket Cloud provider. BaseURL defaults to the public API; "/2.0" is appended when missing.
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.HTTP == nil {
		return nil, errors.New("bitbucket: HTTP client required")
	}
	b := strings.TrimRight(cfg.BaseURL, "/")
	if b == "" {
		b = defaultAPI
	}
	u, err := url.Parse(b)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bitbucket: invalid base_url %q", cfg.BaseURL)
	}
	if !strings.HasSuffix(u.Path, "/2.0") {
		u.Path = strings.TrimRight(u.Path, "/") + "/2.0"
	}
	u.RawPath = ""
	return &bb{base: u, cfg: cfg}, nil
}

func (b *bb) Kind() provider.Kind { return provider.Bitbucket }
func (b *bb) APIHost() string     { return b.base.Host }

// GitUsername implements provider.Provider; Atlassian's documented username for git over HTTPS with an API token.
func (b *bb) GitUsername() string { return "x-bitbucket-api-token-auth" }

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *APIError) Error() string { return fmt.Sprintf("bitbucket: HTTP %d: %s", e.Status, e.Message) }

// endpoint builds an API URL; each segment is escaped as one path element.
func (b *bb) endpoint(query string, segments ...string) string {
	u := *b.base
	decoded, raw := u.Path, u.Path
	for _, s := range segments {
		decoded += "/" + s
		raw += "/" + url.PathEscape(s)
	}
	u.Path, u.RawPath, u.RawQuery = decoded, raw, query
	return u.String()
}

// get performs an authenticated GET; absolute URLs must stay on the API host so the token never leaves it.
func (b *bb) get(ctx context.Context, target string) (*httpx.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if u.Host != b.base.Host || u.Scheme != b.base.Scheme {
		return nil, fmt.Errorf("bitbucket: refusing to send credentials to %s", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.cfg.Token.Reveal())
	resp, err := b.cfg.HTTP.Do(ctx, req)
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
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &m) == nil && m.Error.Message != "" {
		return m.Error.Message
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return strings.TrimSpace(string(body))
}

// page is the paginated envelope; Next is an opaque link that is never constructed by us.
type page[T any] struct {
	Values []T    `json:"values"`
	Next   string `json:"next"`
}

// each follows next links from target, calling fn per page.
func each[T any](ctx context.Context, b *bb, target, what string, fn func([]T) bool) error {
	for n := 0; target != ""; n++ {
		if n >= maxPages {
			return fmt.Errorf("bitbucket: too many pages listing %s", what)
		}
		resp, err := b.get(ctx, target)
		if err != nil {
			return err
		}
		var p page[T]
		if err := json.Unmarshal(resp.Body, &p); err != nil {
			return fmt.Errorf("bitbucket: decoding %s: %w", what, err)
		}
		if !fn(p.Values) {
			return nil
		}
		target = p.Next
	}
	return nil
}

// CheckAuth implements provider.Provider; scopes come from X-OAuth-Scopes when Bitbucket sends it, otherwise they are unknown.
func (b *bb) CheckAuth(ctx context.Context) (provider.Auth, error) {
	resp, err := b.get(ctx, b.endpoint("", "user"))
	if err != nil {
		return provider.Auth{}, err
	}
	var u struct {
		Nickname    string `json:"nickname"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(resp.Body, &u); err != nil {
		return provider.Auth{}, fmt.Errorf("bitbucket: decoding /user: %w", err)
	}
	login := u.Nickname
	if login == "" {
		login = u.DisplayName
	}
	if login == "" {
		return provider.Auth{}, errors.New("bitbucket: no user in answer")
	}
	a := provider.Auth{Login: login}
	for _, s := range strings.Split(resp.Header.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			a.Scopes = append(a.Scopes, s)
		}
	}
	a.Warnings = warnings(a)
	return a, nil
}

var writeScope = regexp.MustCompile(`^(write|delete|admin)\b|^(repository|pullrequest):(write|admin)`)

func warnings(a provider.Auth) []string {
	var w []string
	for _, s := range a.Scopes {
		if writeScope.MatchString(s) {
			w = append(w, fmt.Sprintf("token has the write-capable scope %q; repo-keeper only reads. Use an API token with read:repository:bitbucket, read:pullrequest:bitbucket, read:user:bitbucket and read:workspace:bitbucket only", s))
		}
	}
	if len(a.Scopes) == 0 {
		w = append(w, "Bitbucket did not report this token's scopes; make sure it has only the read:* scopes repo-keeper needs")
	}
	return w
}

type repoJSON struct {
	UUID       string `json:"uuid"`
	FullName   string `json:"full_name"`
	SCM        string `json:"scm"`
	IsPrivate  bool   `json:"is_private"`
	MainBranch *struct {
		Name string `json:"name"`
	} `json:"mainbranch"`
	Parent json.RawMessage `json:"parent"`
	Links  struct {
		Clone []struct {
			Name string `json:"name"`
			Href string `json:"href"`
		} `json:"clone"`
	} `json:"links"`
}

// ListRepos implements provider.Provider; Bitbucket has no cross-workspace listing, so it walks the user's workspaces.
func (b *bb) ListRepos(ctx context.Context) ([]provider.Repo, error) {
	var slugs []string
	err := each(ctx, b, b.endpoint("pagelen=100", "user", "workspaces"), "workspaces", func(ws []struct {
		Workspace struct {
			Slug string `json:"slug"`
		} `json:"workspace"`
	}) bool {
		for _, w := range ws {
			if w.Workspace.Slug != "" {
				slugs = append(slugs, w.Workspace.Slug)
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	var out []provider.Repo
	seen := map[string]bool{}
	for _, slug := range slugs {
		q := "role=member&pagelen=" + fmt.Sprint(pageLen)
		err := each(ctx, b, b.endpoint(q, "repositories", slug), "repositories", func(rs []repoJSON) bool {
			for _, r := range rs {
				if repo, ok := toRepo(r); ok && !seen[r.UUID+repo.FullName] {
					seen[r.UUID+repo.FullName] = true
					out = append(out, repo)
				}
			}
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func toRepo(r repoJSON) (provider.Repo, bool) {
	segs := strings.Split(r.FullName, "/")
	if len(segs) != 2 || r.UUID == "" || (r.SCM != "" && r.SCM != "git") {
		return provider.Repo{}, false // Mercurial and malformed entries cannot be synced
	}
	repo := provider.Repo{
		ID: r.UUID, FullName: r.FullName, Namespace: segs[:1], Name: segs[1],
		Private: r.IsPrivate, Fork: len(r.Parent) > 0 && string(r.Parent) != "null",
	}
	if r.MainBranch != nil {
		repo.DefaultBranch = r.MainBranch.Name
	}
	repo.Disabled = repo.DefaultBranch == "" // no commits yet
	for _, l := range r.Links.Clone {
		switch l.Name {
		case "https":
			repo.CloneURL = stripUser(l.Href) // Bitbucket embeds "user@" in the clone URL; the credential comes from askpass
		case "ssh":
			repo.SSHURL = l.Href
		}
	}
	return repo, true
}

func stripUser(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// MergedBranches implements provider.Provider.
func (b *bb) MergedBranches(ctx context.Context, repo provider.Repo, branches []string) (map[string]string, error) {
	if len(repo.Namespace) != 1 {
		return nil, fmt.Errorf("bitbucket: unexpected namespace in %q", repo.FullName)
	}
	ws, slug := repo.Namespace[0], repo.Name
	want := map[string]bool{}
	for _, br := range branches {
		want[br] = true
	}
	found := map[string]string{}
	fields := "next,values.source.branch.name,values.source.commit.hash,values.source.repository.full_name,values.destination.repository.full_name"
	target := b.endpoint("state=MERGED&sort=-updated_on&pagelen=50&fields="+url.QueryEscape(fields), "repositories", ws, slug, "pullrequests")
	type end struct {
		Branch *struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit *struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	type pr struct {
		Source      end `json:"source"`
		Destination end `json:"destination"`
	}
	for n := 0; target != "" && n < prPages && len(found) < len(want); n++ {
		resp, err := b.get(ctx, target)
		if err != nil {
			return nil, err
		}
		var p page[pr]
		if err := json.Unmarshal(resp.Body, &p); err != nil {
			return nil, fmt.Errorf("bitbucket: decoding pull requests: %w", err)
		}
		for _, x := range p.Values {
			s, d := x.Source, x.Destination
			// only PRs from this repository's own branches; a fork's (or a deleted fork's) same-named branch proves nothing
			if s.Branch == nil || s.Commit == nil || s.Repository == nil || d.Repository == nil ||
				!strings.EqualFold(s.Repository.FullName, d.Repository.FullName) || !strings.EqualFold(d.Repository.FullName, repo.FullName) {
				continue
			}
			if _, done := found[s.Branch.Name]; !want[s.Branch.Name] || done {
				continue
			}
			sha, err := b.expand(ctx, ws, slug, s.Commit.Hash)
			if err != nil {
				if errors.Is(err, provider.ErrAuth) {
					return nil, err
				}
				continue // unknown means "not proven merged": fail closed
			}
			found[s.Branch.Name] = sha
		}
		target = p.Next
	}
	return found, nil
}

// expand returns the full commit id; Bitbucket reports pull-request commits as short hashes, and cleanup compares exactly.
func (b *bb) expand(ctx context.Context, ws, slug, hash string) (string, error) {
	hash = strings.ToLower(hash)
	if fullSHA.MatchString(hash) {
		return hash, nil
	}
	if !regexp.MustCompile(`^[0-9a-f]{7,39}$`).MatchString(hash) {
		return "", fmt.Errorf("bitbucket: unusable commit hash %q", hash)
	}
	resp, err := b.get(ctx, b.endpoint("fields=hash", "repositories", ws, slug, "commit", hash))
	if err != nil {
		return "", err
	}
	var c struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(resp.Body, &c); err != nil || !fullSHA.MatchString(strings.ToLower(c.Hash)) || !strings.HasPrefix(strings.ToLower(c.Hash), hash) {
		return "", fmt.Errorf("bitbucket: commit %s did not resolve to a full id", hash)
	}
	return strings.ToLower(c.Hash), nil
}
