// SPDX-License-Identifier: Apache-2.0

// Package provider defines what an SCM platform must offer; implementations live in subpackages.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

// Kind names a platform.
type Kind string

// Supported platform kinds.
const (
	// GitHub is github.com and GitHub Enterprise Server.
	GitHub Kind = "github"
	// GitLab is gitlab.com and self-managed GitLab.
	GitLab Kind = "gitlab"
	// Gitea is Gitea (self-hosted).
	Gitea Kind = "gitea"
	// Forgejo is Forgejo, including Codeberg.
	Forgejo Kind = "forgejo"
	// Bitbucket is Bitbucket Cloud.
	Bitbucket Kind = "bitbucket"
)

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
	// GitUsername is the username git presents together with the token over HTTPS.
	GitUsername() string
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

// Known reports whether a platform kind is registered.
func Known(k Kind) bool { _, ok := factories[k]; return ok }

// Kinds lists registered platform kinds (for error messages).
func Kinds() []string {
	var ks []string
	for k := range factories {
		ks = append(ks, string(k))
	}
	sort.Strings(ks)
	return ks
}

// New builds the provider for kind.
func New(k Kind, cfg Config) (Provider, error) {
	f, ok := factories[k]
	if !ok {
		return nil, fmt.Errorf("provider: unsupported kind %q", k)
	}
	return f(cfg)
}

// Globs: `*` matches within one path level, `**` matches across levels (for nested GitLab groups), `?` one character
// within a level; everything else is literal and matching is case-insensitive. Character classes are rejected.
var globCache sync.Map

// ValidGlob reports why a pattern is unusable ("" error means fine).
func ValidGlob(p string) error {
	if p == "" {
		return errors.New("empty pattern")
	}
	if strings.ContainsAny(p, "[]\\") {
		return errors.New("character classes and escapes are not supported; use * ** ?")
	}
	return nil
}

func compileGlob(p string) *regexp.Regexp {
	if re, ok := globCache.Load(p); ok {
		return re.(*regexp.Regexp)
	}
	var b strings.Builder
	b.WriteString("(?i)^")
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case p[i] == '*':
			b.WriteString("[^/]*")
		case p[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	b.WriteString("$")
	re := regexp.MustCompile(b.String())
	globCache.Store(p, re)
	return re
}

// Matches applies include/exclude globs to a repo's full name. An empty include list means everything;
// exclude always wins; an invalid pattern matches nothing.
func Matches(include, exclude []string, r Repo) bool {
	hit := func(globs []string) bool {
		for _, g := range globs {
			if ValidGlob(g) == nil && compileGlob(g).MatchString(r.FullName) {
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

// CheckNoSymlinks refuses a destination whose existing path components below root are symlinks, so a clone can
// never be redirected outside the clone root (root itself may be a symlink the user chose).
func CheckNoSymlinks(root, dest string) error {
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("provider: %s is outside the clone root", dest)
	}
	cur := root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing further exists, nothing further can redirect
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("provider: %s is a symlink; refusing to clone through it", cur)
		}
	}
	return nil
}

// ValidCloneURL accepts only remote transports we can authenticate and verify: https, ssh and scp-style ssh.
// Plain http/git (cleartext), file:, ext:: helpers and local paths are rejected, as is anything that looks like an option.
func ValidCloneURL(raw string) error {
	if raw == "" || strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, " \t\r\n\x00") {
		return fmt.Errorf("provider: unusable clone URL %q", shown(raw))
	}
	if strings.Contains(raw, "::") {
		return fmt.Errorf("provider: remote-helper URLs are not allowed: %q", shown(raw))
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "ssh") {
			return fmt.Errorf("provider: only https and ssh clone URLs are allowed, got %q", shown(raw))
		}
		return nil
	}
	// scp-like user@host:path
	if at, colon := strings.Index(raw, "@"), strings.Index(raw, ":"); at > 0 && colon > at+1 && !strings.HasPrefix(raw, "/") {
		return nil
	}
	return fmt.Errorf("provider: only https and ssh clone URLs are allowed, got %q", shown(raw))
}

// shown makes a URL safe to put in an error message: credentials removed, control characters dropped, length bounded.
func shown(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		raw = u.Redacted()
	} else if at := strings.LastIndex(raw, "@"); at > 0 && strings.Contains(raw[:at], ":") {
		raw = "***@" + raw[at+1:] // scp-like or unparsable with a password-looking prefix
	}
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	if len(raw) > 80 {
		raw = raw[:80] + "…"
	}
	return raw
}
