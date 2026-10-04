// SPDX-License-Identifier: Apache-2.0

package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/httpx"
)

// DefaultRepo and DefaultAPI name the only place releases are looked up: this project's GitHub releases.
const (
	DefaultRepo = "basmulder03/repo-keeper"
	DefaultAPI  = "https://api.github.com"
)

// Asset is one downloadable file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is a published GitHub release.
type Release struct {
	Tag        string    `json:"tag_name"`
	Body       string    `json:"body"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	URL        string    `json:"html_url"`
	Assets     []Asset   `json:"assets"`
}

// Version parses the tag.
func (r Release) Version() (Version, error) { return ParseVersion(r.Tag) }

// Asset finds a file of the release by exact name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// securityNote matches the release-notes convention for a security fix: a "Security" heading, a "Security:" line
// or a CVE identifier (docs/RELEASING.md).
var securityNote = regexp.MustCompile(`(?im)^\s{0,3}#{1,6}\s*security\b|^\s*[-*]\s*\**security\**\s*:|\bCVE-\d{4}-\d{4,}\b`)

// Urgent reports whether the release notes flag a security fix.
func (r Release) Urgent() bool { return securityNote.MatchString(r.Body) }

// Doer is the part of httpx.Client the update code needs.
type Doer interface {
	Do(ctx context.Context, req *http.Request) (*httpx.Response, error)
}

// Checker asks the GitHub releases API what exists. It sends nothing but the client's standard User-Agent.
type Checker struct {
	HTTP Doer
	API  string // default DefaultAPI
	Repo string // default DefaultRepo
}

func (c Checker) api() string {
	if c.API != "" {
		return strings.TrimRight(c.API, "/")
	}
	return DefaultAPI
}

func (c Checker) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return DefaultRepo
}

// Get performs one GET below the API root and decodes the JSON answer.
func (c Checker) Get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.api() + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HTTP.Do(ctx, req)
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return fmt.Errorf("update: GET %s: HTTP %d", path, resp.Status)
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("update: decoding %s: %w", path, err)
	}
	return nil
}

// Releases lists the newest published releases (drafts are never shown to unauthenticated callers, and are dropped anyway).
func (c Checker) Releases(ctx context.Context) ([]Release, error) {
	var rs []Release
	if err := c.Get(ctx, "/repos/"+c.repo()+"/releases", url.Values{"per_page": {"30"}}, &rs); err != nil {
		return nil, err
	}
	out := rs[:0]
	for _, r := range rs {
		if !r.Draft {
			out = append(out, r)
		}
	}
	return out, nil
}

// ErrNoRelease means no published release is newer than, or eligible for, the running version.
var ErrNoRelease = errors.New("update: no eligible release")

// Newest picks the highest published version. A running pre-release also considers pre-releases; a running final
// release only considers final ones, so a stable install is never nudged onto a beta.
func Newest(rs []Release, current Version) (Release, Version, error) {
	var best Release
	var bv Version
	found := false
	for _, r := range rs {
		v, err := r.Version()
		if err != nil || r.Draft || ((r.Prerelease || v.Prerelease()) && !current.Prerelease()) {
			continue
		}
		if !found || v.Compare(bv) > 0 {
			best, bv, found = r, v, true
		}
	}
	if !found {
		return Release{}, Version{}, ErrNoRelease
	}
	return best, bv, nil
}

// Find returns the published release with exactly this version (for an explicit --version).
func Find(rs []Release, want Version) (Release, bool) {
	for _, r := range rs {
		if v, err := r.Version(); err == nil && v.Compare(want) == 0 && !r.Draft {
			return r, true
		}
	}
	return Release{}, false
}
