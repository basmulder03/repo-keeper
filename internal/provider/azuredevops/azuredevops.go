// SPDX-License-Identifier: Apache-2.0

// Package azuredevops implements provider.Provider for Azure DevOps Services and Server (REST API 7.1).
package azuredevops

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
	apiVersion = "7.1"
	maxPages   = 500
	prPages    = 3
	pageSize   = 100
)

func init() { provider.Register(provider.AzureDevOps, New) }

type ado struct {
	base *url.URL
	org  string
	cfg  provider.Config
}

// New builds an Azure DevOps provider. BaseURL is required and is the organization (https://dev.azure.com/acme)
// or, for Server, the collection (https://tfs.example.com/tfs/DefaultCollection).
func New(cfg provider.Config) (provider.Provider, error) {
	if cfg.HTTP == nil {
		return nil, errors.New("azuredevops: HTTP client required")
	}
	b := strings.TrimRight(cfg.BaseURL, "/")
	if b == "" {
		return nil, errors.New("azuredevops: base_url is required (your organization, e.g. https://dev.azure.com/acme)")
	}
	u, err := url.Parse(b)
	if err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return nil, fmt.Errorf("azuredevops: base_url %q must include the organization, e.g. https://dev.azure.com/acme", cfg.BaseURL)
	}
	u.RawPath = ""
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	return &ado{base: u, org: segs[len(segs)-1], cfg: cfg}, nil
}

func (a *ado) Kind() provider.Kind { return provider.AzureDevOps }
func (a *ado) APIHost() string     { return a.base.Host }

// GitUsername implements provider.Provider; Azure DevOps ignores the user name when a PAT is the password.
func (a *ado) GitUsername() string { return "pat" }

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("azuredevops: HTTP %d: %s", e.Status, e.Message)
}

// endpoint builds an API URL below the organization; each segment is escaped as one path element.
func (a *ado) endpoint(query url.Values, segments ...string) string {
	u := *a.base
	decoded, raw := u.Path, u.Path
	for _, s := range segments {
		decoded += "/" + s
		raw += "/" + url.PathEscape(s)
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", apiVersion)
	u.Path, u.RawPath, u.RawQuery = decoded, raw, query.Encode()
	return u.String()
}

// get performs an authenticated GET and returns the response; it must stay on the organization's host.
func (a *ado) get(ctx context.Context, target string) (*httpx.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if u.Host != a.base.Host || u.Scheme != a.base.Scheme {
		return nil, fmt.Errorf("azuredevops: refusing to send credentials to %s", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth("", a.cfg.Token.Reveal())
	resp, err := a.cfg.HTTP.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden && resp.Header.Get("X-TFS-FedAuthRedirect") != "":
		return nil, fmt.Errorf("%w: %s", provider.ErrAuth, message(resp.Body))
	// an invalid or expired PAT is often answered with 203 and the HTML sign-in page instead of 401
	case resp.Status == http.StatusNonAuthoritativeInfo || strings.Contains(resp.Header.Get("Content-Type"), "text/html"):
		return nil, fmt.Errorf("%w: Azure DevOps answered with a sign-in page (token invalid, expired, or without access to this organization)", provider.ErrAuth)
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

type project struct {
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// projects lists every project of the organization (continuation token in the x-ms-continuationtoken header).
func (a *ado) projects(ctx context.Context, top int) ([]project, error) {
	var out []project
	token := ""
	for n := 0; ; n++ {
		if n >= maxPages {
			return nil, errors.New("azuredevops: too many pages listing projects")
		}
		q := url.Values{"$top": {strconv.Itoa(top)}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		resp, err := a.get(ctx, a.endpoint(q, "_apis", "projects"))
		if err != nil {
			return nil, err
		}
		var p struct {
			Value []project `json:"value"`
		}
		if err := json.Unmarshal(resp.Body, &p); err != nil {
			return nil, fmt.Errorf("azuredevops: decoding projects: %w", err)
		}
		out = append(out, p.Value...)
		if token = resp.Header.Get("x-ms-continuationtoken"); token == "" || top == 1 {
			return out, nil
		}
	}
}

// CheckAuth implements provider.Provider; the documented API cannot tell which user a PAT belongs to, so success
// means "this token can read projects of this organization" and the login is the organization.
func (a *ado) CheckAuth(ctx context.Context) (provider.Auth, error) {
	if _, err := a.projects(ctx, 1); err != nil {
		return provider.Auth{}, err
	}
	return provider.Auth{Login: a.org, Warnings: []string{
		"Azure DevOps does not report a token's owner, scopes or expiry; use an organization-scoped PAT with only Code (Read) and Project and Team (Read), and a short expiry",
	}}, nil
}

type repoJSON struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	RemoteURL     string `json:"remoteUrl"`
	SSHURL        string `json:"sshUrl"`
	IsDisabled    bool   `json:"isDisabled"`
	IsFork        bool   `json:"isFork"`
	Project       struct {
		Name string `json:"name"`
	} `json:"project"`
}

// ListRepos implements provider.Provider; the documented list needs a project, so every project is walked.
func (a *ado) ListRepos(ctx context.Context) ([]provider.Repo, error) {
	ps, err := a.projects(ctx, pageSize)
	if err != nil {
		return nil, err
	}
	var out []provider.Repo
	seen := map[string]bool{}
	for _, p := range ps {
		resp, err := a.get(ctx, a.endpoint(nil, p.Name, "_apis", "git", "repositories"))
		if err != nil {
			return nil, err
		}
		var rs struct {
			Value []repoJSON `json:"value"`
		}
		if err := json.Unmarshal(resp.Body, &rs); err != nil {
			return nil, fmt.Errorf("azuredevops: decoding repositories of %q: %w", p.Name, err)
		}
		for _, r := range rs.Value {
			if r.ID == "" || r.Name == "" || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			branch := strings.TrimPrefix(r.DefaultBranch, "refs/heads/")
			out = append(out, provider.Repo{
				ID: r.ID, FullName: p.Name + "/" + r.Name, Namespace: []string{p.Name}, Name: r.Name,
				CloneURL: stripUser(r.RemoteURL), SSHURL: r.SSHURL, DefaultBranch: branch,
				Private: p.Visibility != "public", Fork: r.IsFork,
				Disabled: r.IsDisabled || branch == "", // disabled, or nothing to sync yet
			})
		}
	}
	return out, nil
}

// stripUser removes the "org@" Azure DevOps embeds in clone URLs; credentials come only from askpass.
func stripUser(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// MergedBranches implements provider.Provider; repo.FullName is "project/repository".
func (a *ado) MergedBranches(ctx context.Context, repo provider.Repo, branches []string) (map[string]string, error) {
	proj, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || proj == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("azuredevops: unexpected repository name %q", repo.FullName)
	}
	want := map[string]bool{}
	for _, b := range branches {
		want[b] = true
	}
	found := map[string]string{}
	skip := 0
	for n := 0; n < prPages && len(found) < len(want); n++ {
		q := url.Values{"searchCriteria.status": {"completed"}, "$top": {strconv.Itoa(pageSize)}, "$skip": {strconv.Itoa(skip)}}
		resp, err := a.get(ctx, a.endpoint(q, proj, "_apis", "git", "repositories", name, "pullrequests"))
		if err != nil {
			return nil, err
		}
		var prs struct {
			Value []struct {
				Status                string          `json:"status"`
				SourceRefName         string          `json:"sourceRefName"`
				ForkSource            json.RawMessage `json:"forkSource"`
				LastMergeSourceCommit *struct {
					CommitID string `json:"commitId"`
				} `json:"lastMergeSourceCommit"`
			} `json:"value"`
		}
		if err := json.Unmarshal(resp.Body, &prs); err != nil {
			return nil, fmt.Errorf("azuredevops: decoding pull requests: %w", err)
		}
		for _, p := range prs.Value {
			branch, isHead := strings.CutPrefix(p.SourceRefName, "refs/heads/")
			// only PRs from this repository's own branches; a fork's same-named branch proves nothing
			fromFork := len(p.ForkSource) > 0 && string(p.ForkSource) != "null"
			if p.Status != "completed" || !isHead || fromFork || p.LastMergeSourceCommit == nil || !fullSHA.MatchString(p.LastMergeSourceCommit.CommitID) {
				continue
			}
			if _, done := found[branch]; want[branch] && !done {
				found[branch] = p.LastMergeSourceCommit.CommitID
			}
		}
		if len(prs.Value) < pageSize {
			break
		}
		skip += len(prs.Value)
	}
	return found, nil
}
