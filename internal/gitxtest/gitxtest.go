// SPDX-License-Identifier: Apache-2.0

// Package gitxtest builds throw-away git fixtures (bare origin + clones) fully isolated from the real HOME.
package gitxtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/gitx"
)

// Env is an isolated origin + working clone pair.
type Env struct {
	T      *testing.T
	R      *gitx.Runner
	Root   string
	Origin string // bare repository
	Work   string // clone of Origin on "main"
}

// New creates the fixture with one commit on main, pushed to origin.
func New(t *testing.T) *Env {
	t.Helper()
	root := t.TempDir()
	r, err := gitx.New()
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	r.Env = []string{
		"HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "xdg"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	}
	e := &Env{T: t, R: r, Root: root, Origin: filepath.Join(root, "origin.git"), Work: filepath.Join(root, "work")}
	e.Git(root, "init", "--bare", "-q", "-b", "main", e.Origin)
	e.Git(root, "clone", "-q", e.Origin, e.Work)
	e.Git(e.Work, "checkout", "-q", "-B", "main")
	e.Commit(e.Work, "README.md", "hello\n", "initial")
	e.Git(e.Work, "push", "-q", "-u", "origin", "main")
	e.Git(e.Work, "remote", "set-head", "origin", "main")
	return e
}

// Git runs git in dir and fails the test on error.
func (e *Env) Git(dir string, args ...string) string {
	e.T.Helper()
	out, err := e.R.Run(context.Background(), dir, args...)
	if err != nil {
		e.T.Fatalf("git %s (in %s): %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(out)
}

// Commit writes file, commits it, and returns the new SHA.
func (e *Env) Commit(dir, file, content, msg string) string {
	e.T.Helper()
	p := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		e.T.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		e.T.Fatal(err)
	}
	e.Git(dir, "add", "--", file)
	e.Git(dir, "commit", "-q", "-m", msg)
	return e.Git(dir, "rev-parse", "HEAD")
}

// Clone makes another clone of origin (a "teammate") and returns its path.
func (e *Env) Clone(name string) string {
	e.T.Helper()
	dir := filepath.Join(e.Root, name)
	e.Git(e.Root, "clone", "-q", e.Origin, dir)
	return dir
}

// Repo returns the gitx.Repo for dir.
func (e *Env) Repo(dir string) *gitx.Repo { return e.R.Repo(dir) }
