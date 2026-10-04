// SPDX-License-Identifier: Apache-2.0

// Package apply downloads, verifies and installs a release over the running binary. It is the only code in the
// project that replaces an executable, and by ADR-0021 only the explicit `repo-keeper update` command may import it:
// the daemon, scheduler, web UI and tray never can (enforced by an architecture test).
package apply

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/basmulder03/repo-keeper/internal/update"
)

// Issuer is the OIDC issuer GitHub Actions signs with; the identity is pinned per repository and tag.
const Issuer = "https://token.actions.githubusercontent.com"

// Verifier checks the cosign signature of checksums.txt.
type Verifier interface {
	Verify(ctx context.Context, checksums, sig, cert []byte, identity, issuer string) error
}

// Signature is the outcome of the signature check, kept so --check can show it without failing.
type Signature struct {
	Verified bool
	Detail   string
}

// Source says where release files come from.
type Source struct {
	update.Checker
	Download string // asset host, default https://github.com
	Verifier Verifier
	GOOS     string
	GOARCH   string
}

func (s Source) download() string {
	if s.Download != "" {
		return strings.TrimRight(s.Download, "/")
	}
	return "https://github.com"
}

// Evidence is everything a person needs to judge a release without trusting this program.
type Evidence struct {
	Repo        string
	Release     update.Release
	Version     update.Version
	Tag         string
	ArchiveName string
	Archive     update.Asset
	ChecksumsAt string
	Checksums   []byte
	Sig, Cert   []byte
	ExpectedSHA string
	Computed    string // set after the archive was downloaded
	Identity    string
	Issuer      string
	Signature   Signature
	Commit      string // best effort: the commit the tag points at
	RunURL      string // best effort: the release workflow run for that commit
}

// ArchiveName is the release archive for a platform; only Linux amd64 and arm64 are published and signed.
func ArchiveName(version, goos, goarch string) (string, error) {
	if goos != "linux" || (goarch != "amd64" && goarch != "arm64") {
		return "", fmt.Errorf("no signed release is published for %s/%s (Linux amd64 and arm64 only)", goos, goarch)
	}
	return fmt.Sprintf("repo-keeper_%s_linux_%s.tar.gz", version, goarch), nil
}

// Gather downloads the small files (checksums, signature, certificate), verifies the signature and extracts what is
// expected of the archive. It does not download the archive and never fails just because the signature does not verify.
func (s Source) Gather(ctx context.Context, rel update.Release) (Evidence, error) {
	v, err := rel.Version()
	if err != nil {
		return Evidence{}, err
	}
	repo := s.Repo
	if repo == "" {
		repo = update.DefaultRepo
	}
	name, err := ArchiveName(v.String(), s.GOOS, s.GOARCH)
	if err != nil {
		return Evidence{}, err
	}
	ev := Evidence{
		Repo: repo, Release: rel, Version: v, Tag: rel.Tag, ArchiveName: name,
		Identity: fmt.Sprintf("https://github.com/%s/.github/workflows/release.yml@refs/tags/%s", repo, rel.Tag), Issuer: Issuer,
	}
	prefix := s.download() + "/" + repo + "/releases/download/" + rel.Tag + "/"
	pick := func(file string) (update.Asset, error) {
		a, ok := rel.Asset(file)
		if !ok {
			return a, fmt.Errorf("release %s has no %s", rel.Tag, file)
		}
		// only files of this very release on the official download host, whatever the API answer says
		if !strings.HasPrefix(a.URL, prefix) || strings.ContainsAny(strings.TrimPrefix(a.URL, prefix), "/?#") {
			return a, fmt.Errorf("unexpected download URL for %s: %s", file, a.URL)
		}
		return a, nil
	}
	if ev.Archive, err = pick(name); err != nil {
		return ev, err
	}
	files := map[string]*[]byte{"checksums.txt": &ev.Checksums, "checksums.txt.sig": &ev.Sig, "checksums.txt.pem": &ev.Cert}
	for file, dst := range files {
		a, err := pick(file)
		if err != nil {
			return ev, err
		}
		if file == "checksums.txt" {
			ev.ChecksumsAt = a.URL
		}
		if *dst, err = s.fetch(ctx, a.URL); err != nil {
			return ev, err
		}
	}
	if ev.ExpectedSHA, err = expectedSHA(ev.Checksums, name); err != nil {
		return ev, err
	}
	if s.Verifier == nil {
		ev.Signature = Signature{Detail: "NOT verified: no verifier configured"}
	} else if err := s.Verifier.Verify(ctx, ev.Checksums, ev.Sig, ev.Cert, ev.Identity, ev.Issuer); err != nil {
		ev.Signature = Signature{Detail: "NOT verified: " + err.Error()}
	} else {
		ev.Signature = Signature{Verified: true, Detail: "verified"}
	}
	s.provenance(ctx, &ev)
	return ev, nil
}

func (s Source) fetch(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, fmt.Errorf("download %s: HTTP %d", u, resp.Status)
	}
	return resp.Body, nil
}

// expectedSHA reads "<sha256>  <file>" lines; the archive must be listed exactly once.
func expectedSHA(checksums []byte, name string) (string, error) {
	var found string
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == name {
			if found != "" {
				return "", fmt.Errorf("%s is listed twice in checksums.txt", name)
			}
			if len(f[0]) != 64 {
				return "", fmt.Errorf("checksums.txt has a malformed hash for %s", name)
			}
			found = strings.ToLower(f[0])
		}
	}
	if found == "" {
		return "", fmt.Errorf("%s is not listed in checksums.txt", name)
	}
	return found, nil
}

// provenance looks up the commit of the tag and the workflow run that built it. Both are conveniences for the reader;
// trust comes from the signature, so failures here only leave the fields empty.
func (s Source) provenance(ctx context.Context, ev *Evidence) {
	var c struct {
		SHA string `json:"sha"`
	}
	if s.Get(ctx, "/repos/"+ev.Repo+"/commits/"+url.PathEscape(ev.Tag), nil, &c) != nil || c.SHA == "" {
		return
	}
	ev.Commit = c.SHA
	var runs struct {
		Runs []struct {
			Name string `json:"name"`
			URL  string `json:"html_url"`
		} `json:"workflow_runs"`
	}
	if s.Get(ctx, "/repos/"+ev.Repo+"/actions/runs", url.Values{"head_sha": {c.SHA}, "per_page": {"20"}}, &runs) != nil {
		return
	}
	for _, r := range runs.Runs {
		if r.Name == "release" {
			ev.RunURL = r.URL
			return
		}
	}
}

// Fetch downloads the archive and refuses it unless its SHA-256 equals the one listed in the (verified) checksums.txt.
func (s Source) Fetch(ctx context.Context, ev *Evidence) ([]byte, error) {
	body, err := s.fetch(ctx, ev.Archive.URL)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	ev.Computed = hex.EncodeToString(sum[:])
	if ev.Computed != ev.ExpectedSHA {
		return nil, fmt.Errorf("SHA-256 mismatch for %s: expected %s, got %s", ev.ArchiveName, ev.ExpectedSHA, ev.Computed)
	}
	return body, nil
}

// ErrNoCosign means the cosign program is not installed, so the signature cannot be checked.
var ErrNoCosign = errors.New("cosign is not installed")
