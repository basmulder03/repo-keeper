// SPDX-License-Identifier: Apache-2.0

// Package gitlab implements provider.Provider for gitlab.com and self-managed GitLab (REST API v4).
package gitlab

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
	defaultAPI = "https://gitlab.com/api/v4"
	maxPages   = 500
	mrPages    = 3
)

func init() { provider.Register(provider.GitLab, New) }

type gl struct {
	base *url.URL
	cfg  provider.Config
}

// New builds a GitLab provider. BaseURL may be the instance root (https://gitlab.example.com) or the full
// API URL; "/api/v4" is appended when missing.
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.HTTP == nil {
		return nil, errors.New("gitlab: HTTP client required")
	}
	b := strings.TrimRight(cfg.BaseURL, "/")
	if b == "" {
		b = defaultAPI
	}
	u, err := url.Parse(b)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("gitlab: invalid base_url %q", cfg.BaseURL)
	}
	if !strings.HasSuffix(u.Path, "/api/v4") {
		u.Path = strings.TrimRight(u.Path, "/") + "/api/v4"
	}
	u.RawPath = ""
	return &gl{base: u, cfg: cfg}, nil
}

func (g *gl) Kind() provider.Kind { return provider.GitLab }
func (g *gl) APIHost() string     { return g.base.Host }

// GitUsername implements provider.Provider; GitLab accepts any non-empty user with a token, "oauth2" is its convention.
func (g *gl) GitUsername() string { return "oauth2" }

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *APIError) Error() string { return fmt.Sprintf("gitlab: HTTP %d: %s", e.Status, e.Message) }

// endpoint builds an API URL; segments are path elements (escaped here, so "group/sub/proj" stays one %2F-encoded element).
func (g *gl) endpoint(query string, segments ...string) string {
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
func (g *gl) get(ctx context.Context, target string) (*httpx.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if u.Host != g.base.Host || u.Scheme != g.base.Scheme {
		return nil, fmt.Errorf("gitlab: refusing to send credentials to %s", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
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

func message(body []byte) string {
	var m struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if json.Unmarshal(body, &m) == nil {
		switch {
		case m.Desc != "":
			return m.Desc
		case m.Error != "":
			return m.Error
		case m.Message != nil:
			return fmt.Sprint(m.Message)
		}
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

func (g *gl) CheckAuth(ctx context.Context) (provider.Auth, error) {
	resp, err := g.get(ctx, g.endpoint("", "user"))
	if err != nil {
		return provider.Auth{}, err
	}
	var u struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(resp.Body, &u); err != nil {
		return provider.Auth{}, fmt.Errorf("gitlab: decoding /user: %w", err)
	}
	a := provider.Auth{Login: u.Username}

	// Token metadata exists for personal access tokens; OAuth/job tokens answer 401/404, which is fine.
	if r, err := g.get(ctx, g.endpoint("", "personal_access_tokens", "self")); err == nil {
		var t struct {
			Scopes    []string `json:"scopes"`
			ExpiresAt string   `json:"expires_at"`
		}
		if json.Unmarshal(r.Body, &t) == nil {
			a.Scopes = t.Scopes
			if t.ExpiresAt != "" {
				if ex, err := time.Parse("2006-01-02", t.ExpiresAt); err == nil {
					a.Expires = ex
				}
			}
		}
	}
	a.Warnings = warnings(a)
	return a, nil
}

func warnings(a provider.Auth) []string {
	var w []string
	for _, s := range a.Scopes {
		switch s {
		case "api", "write_repository", "write_registry", "sudo", "admin_mode", "create_runner", "manage_runner":
			w = append(w, fmt.Sprintf("token has the broad %q scope; repo-keeper only reads. Prefer read_api + read_repository (a project or group access token with the Reporter role is even narrower)", s))
		}
	}
	if !a.Expires.IsZero() && time.Until(a.Expires) < 14*24*time.Hour {
		w = append(w, "token expires "+a.Expires.Format("2006-01-02"))
	}
	return w
}

type projectJSON struct {
	ID                int64           `json:"id"`
	Path              string          `json:"path"`
	PathWithNamespace string          `json:"path_with_namespace"`
	DefaultBranch     string          `json:"default_branch"`
	HTTPURL           string          `json:"http_url_to_repo"`
	SSHURL            string          `json:"ssh_url_to_repo"`
	Archived          bool            `json:"archived"`
	ForkedFrom        json.RawMessage `json:"forked_from_project"`
	EmptyRepo         bool            `json:"empty_repo"`
	Visibility        string          `json:"visibility"`
}

func (g *gl) ListRepos(ctx context.Context) ([]provider.Repo, error) {
	var out []provider.Repo
	// keyset pagination is stable under concurrent changes and cheap for the server
	target := g.endpoint("membership=true&per_page=100&order_by=id&sort=asc&pagination=keyset&statistics=false", "projects")
	for page := 0; target != ""; page++ {
		if page >= maxPages {
			return nil, errors.New("gitlab: too many pages listing projects")
		}
		resp, err := g.get(ctx, target)
		if err != nil {
			return nil, err
		}
		var ps []projectJSON
		if err := json.Unmarshal(resp.Body, &ps); err != nil {
			return nil, fmt.Errorf("gitlab: decoding projects: %w", err)
		}
		for _, p := range ps {
			segs := strings.Split(p.PathWithNamespace, "/")
			if len(segs) < 2 {
				continue
			}
			isFork := len(p.ForkedFrom) > 0 && string(p.ForkedFrom) != "null"
			out = append(out, provider.Repo{
				ID: strconv.FormatInt(p.ID, 10), FullName: p.PathWithNamespace,
				Namespace: segs[:len(segs)-1], Name: segs[len(segs)-1],
				CloneURL: p.HTTPURL, SSHURL: p.SSHURL, DefaultBranch: p.DefaultBranch,
				Private: p.Visibility != "public", Archived: p.Archived, Fork: isFork,
				Disabled: p.EmptyRepo, // nothing to sync yet
			})
		}
		target = next(resp.Header)
	}
	return out, nil
}

func (g *gl) MergedBranches(ctx context.Context, repo provider.Repo, branches []string) (map[string]string, error) {
	want := map[string]bool{}
	for _, b := range branches {
		want[b] = true
	}
	found := map[string]string{}
	target := g.endpoint("state=merged&scope=all&order_by=updated_at&sort=desc&per_page=100", "projects", repo.FullName, "merge_requests")
	for page := 0; target != "" && page < mrPages && len(found) < len(want); page++ {
		resp, err := g.get(ctx, target)
		if err != nil {
			return nil, err
		}
		var mrs []struct {
			SourceBranch    string `json:"source_branch"`
			SHA             string `json:"sha"`
			SourceProjectID int64  `json:"source_project_id"`
			TargetProjectID int64  `json:"target_project_id"`
			MergedAt        string `json:"merged_at"`
		}
		if err := json.Unmarshal(resp.Body, &mrs); err != nil {
			return nil, fmt.Errorf("gitlab: decoding merge requests: %w", err)
		}
		for _, m := range mrs {
			// only MRs from this project's own branches; a fork's same-named branch proves nothing
			if m.MergedAt == "" || m.SHA == "" || m.SourceProjectID != m.TargetProjectID {
				continue
			}
			if _, done := found[m.SourceBranch]; want[m.SourceBranch] && !done {
				found[m.SourceBranch] = m.SHA
			}
		}
		target = next(resp.Header)
	}
	return found, nil
}
