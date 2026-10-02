// SPDX-License-Identifier: Apache-2.0

// Package provider defines what an SCM platform must offer; implementations live in subpackages.
package provider

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

// Kind names a platform.
type Kind string

// GitHub is github.com and GitHub Enterprise Server.
const GitHub Kind = "github"

// ErrAuth means the credential is missing, revoked or expired; retrying will not help until it is replaced.
var ErrAuth = errors.New("provider: authentication failed")

// Repo is a remote repository as reported by a platform.
type Repo struct {
	ID            string
	FullName      string // "namespace/name", namespace may have several levels on other platforms
	Namespace     []string
	Name          string
	CloneURL      string // https
	SSHURL        string
	DefaultBranch string
	Private       bool
	Archived      bool
	Fork          bool
	Disabled      bool
}

// Auth describes a verified credential.
type Auth struct {
	Login    string
	Scopes   []string
	Expires  time.Time // zero = unknown/never
	Warnings []string
}

// Provider is one authenticated account on one platform.
type Provider interface {
	Kind() Kind
	// APIHost is the rate-limit key of the API endpoint.
	APIHost() string
	// CheckAuth validates the credential and reports scopes, expiry and over-privilege warnings.
	CheckAuth(ctx context.Context) (Auth, error)
	// ListRepos returns every repository the account can access.
	ListRepos(ctx context.Context) ([]Repo, error)
	// MergedBranches maps branch -> head SHA of its merged PR/MR, for the given branches of repo.
	MergedBranches(ctx context.Context, repo Repo, branches []string) (map[string]string, error)
}

// Config builds a Provider.
type Config struct {
	BaseURL string
	Token   secrets.Token
	HTTP    *httpx.Client
}

// Factory creates a Provider from Config.
type Factory func(Config) (Provider, error)

var factories = map[Kind]Factory{}

// Register makes a platform available by kind (called from init of each subpackage's importer).
func Register(k Kind, f Factory) { factories[k] = f }

// New builds the provider for kind.
func New(k Kind, cfg Config) (Provider, error) {
	f, ok := factories[k]
	if !ok {
		return nil, fmt.Errorf("provider: unsupported kind %q", k)
	}
	return f(cfg)
}

// Matches applies include/exclude globs to a repo's full name (case-insensitive; "*" does not cross "/").
// An empty include list means everything; exclude always wins.
func Matches(include, exclude []string, r Repo) bool {
	name := strings.ToLower(r.FullName)
	hit := func(globs []string) bool {
		for _, g := range globs {
			if ok, err := path.Match(strings.ToLower(g), name); err == nil && ok {
				return true
			}
		}
		return false
	}
	if hit(exclude) {
		return false
	}
	return len(include) == 0 || hit(include)
}

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LocalPath maps a repo to <root>/<kind>/<namespace...>/<name> and refuses anything that could escape root.
func LocalPath(root string, k Kind, r Repo) (string, error) {
	segs := append(append([]string{string(k)}, r.Namespace...), r.Name)
	for _, s := range segs {
		if !safeSegment.MatchString(s) || strings.HasSuffix(s, ".") {
			return "", fmt.Errorf("provider: unsafe path segment %q in %q", s, r.FullName)
		}
	}
	p := filepath.Join(append([]string{root}, segs...)...)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("provider: path for %q escapes the clone root", r.FullName)
	}
	return p, nil
}
