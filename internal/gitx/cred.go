// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/basmulder03/repo-keeper/internal/askpass"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

// Cred is an HTTPS credential valid for exactly one host. It reaches git only through the child's
// environment via the askpass helper: never argv, URLs, config files or logs.
type Cred struct {
	Host     string // host[:port] the secret may be sent to
	Username string
	Secret   secrets.Token
	// Helper is the executable git calls for prompts (default: this binary, which switches mode on EnvMarker).
	Helper string
	// CAFile is a PEM bundle git should trust for this host (self-hosted instance with a private CA); not a secret.
	CAFile string
}

func (c *Cred) env() ([]string, error) {
	if c == nil || c.Secret.IsZero() {
		return nil, nil
	}
	helper := c.Helper
	if helper == "" {
		var err error
		if helper, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("gitx: locating askpass helper: %w", err)
		}
	}
	env := []string{
		"GIT_ASKPASS=" + helper,
		askpass.EnvMarker + "=1",
		askpass.EnvHost + "=" + c.Host,
		askpass.EnvUser + "=" + c.Username,
		askpass.EnvSecret + "=" + c.Secret.Reveal(),
	}
	if c.CAFile != "" {
		env = append(env, "GIT_SSL_CAINFO="+c.CAFile)
	}
	return env, nil
}

// WithCred returns a copy of g that authenticates network operations with c.
func (g *Repo) WithCred(c *Cred) *Repo {
	cp := *g
	cp.Cred = c
	return &cp
}

// Clone clones url into dest (which must not exist) without running hooks or touching submodules.
func (r *Runner) Clone(ctx context.Context, url, dest string, cred *Cred, blobless bool) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("gitx: %s already exists", dest)
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("gitx: %w", err)
	}
	args := []string{"clone", "--no-recurse-submodules", "--origin", "origin"}
	if blobless {
		args = append(args, "--filter=blob:none")
	}
	args = append(args, "--", url, filepath.Base(dest))
	_, err := (&Repo{R: r, Dir: parent, Cred: cred}).run(ctx, args...)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		_ = os.RemoveAll(dest) // never leave a half-cloned directory that later looks like a repo
	}
	return err
}
