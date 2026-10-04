// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

// installVersion is the release the generated commands pin: the newest published one, so a copy-paste never points
// at a draft. Without release data it falls back to VERSION and says that the release may not be published yet.
func (b *build) installVersion() (version string, published bool, err error) {
	rs, err := b.loadReleases()
	if err != nil {
		return "", false, err
	}
	if len(rs) > 0 {
		return strings.TrimPrefix(rs[0].Tag, "v"), true, nil
	}
	return b.Version, false, nil
}

// genInstall publishes scripts/install.sh with the pinned version filled in, and appends ready-to-paste commands.
func genInstall(b *build) (string, error) {
	v, published, err := b.installVersion()
	if err != nil {
		return "", err
	}
	src, err := b.read("scripts/install.sh")
	if err != nil {
		return "", err
	}
	// only the default in the assignment is pinned; the "unpinned copy" guard further down must keep its placeholder
	const assign = `VERSION="${REPO_KEEPER_VERSION:-@VERSION@}"`
	if strings.Count(string(src), assign) != 1 {
		return "", fmt.Errorf("install.sh no longer contains the expected version assignment %s", assign)
	}
	script := strings.Replace(string(src), assign, `VERSION="${REPO_KEEPER_VERSION:-`+v+`}"`, 1)
	if err := os.WriteFile(filepath.Join(b.Out, "install.sh"), []byte(script), 0o600); err != nil { //nolint:gosec // served as a static file
		return "", err
	}
	sum := sha256.Sum256([]byte(script))
	sha := hex.EncodeToString(sum[:])
	tag, base := "v"+v, b.RepoURL+"/releases/download/v"+v
	esc := html.EscapeString

	var h strings.Builder
	h.WriteString("<h2 id=\"pinned\">Ready to paste, pinned to " + esc(tag) + "</h2>\n")
	if published {
		h.WriteString("<p>These commands pin the newest published release, <code>" + esc(tag) + "</code>.</p>\n")
	} else {
		h.WriteString("<p class=\"muted\"><strong>No release is published yet.</strong> The commands below pin <code>" + esc(tag) + "</code> (the current <code>VERSION</code>) and start working as soon as that release is published; the Nix commands work from source already.</p>\n")
	}
	h.WriteString("<h3 id=\"pinned-nix\">Nix</h3>\n<pre><code>nix run " + esc("github:basmulder03/repo-keeper/"+tag) + " -- version\nnix profile add " + esc("github:basmulder03/repo-keeper/"+tag) + "</code></pre>\n")
	h.WriteString("<h3 id=\"pinned-script\">Any Linux (amd64, arm64)</h3>\n<pre><code>curl -fsSLo install.sh https://basmulder03.github.io/repo-keeper/install.sh\necho '" + sha + "  install.sh' | sha256sum --check\nsh install.sh</code></pre>\n")
	h.WriteString("<p>SHA-256 of the script published here: <code>" + sha + "</code>. It pins <code>" + esc(tag) + "</code>, verifies the signature and the archive checksum before installing, and refuses on any mismatch. <a href=\"install.sh\">Read it first</a>.</p>\n")
	h.WriteString("<h3 id=\"pinned-deb\">Debian and Ubuntu</h3>\n<pre><code>" + esc(fmt.Sprintf(`a=$(dpkg --print-architecture)   # amd64 or arm64
for f in repo-keeper_%[1]s_linux_${a}.deb checksums.txt checksums.txt.sig checksums.txt.pem; do curl -fsSLO %[2]s/$f; done
cosign verify-blob checksums.txt --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity %[3]s/.github/workflows/release.yml@refs/tags/%[4]s \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt
sudo apt install ./repo-keeper_%[1]s_linux_${a}.deb`, v, base, b.RepoURL, tag)) + "</code></pre>\n")
	h.WriteString("<h3 id=\"pinned-rpm\">Fedora and RHEL</h3>\n<pre><code>" + esc(fmt.Sprintf(`a=$(uname -m); [ "$a" = x86_64 ] && a=amd64; [ "$a" = aarch64 ] && a=arm64
for f in repo-keeper_%[1]s_linux_${a}.rpm checksums.txt checksums.txt.sig checksums.txt.pem; do curl -fsSLO %[2]s/$f; done
cosign verify-blob checksums.txt --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity %[3]s/.github/workflows/release.yml@refs/tags/%[4]s \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt
sudo dnf install ./repo-keeper_%[1]s_linux_${a}.rpm`, v, base, b.RepoURL, tag)) + "</code></pre>\n")
	return h.String(), nil
}
