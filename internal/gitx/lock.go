// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrLocked means another repo-keeper operation holds this repository.
var ErrLocked = errors.New("gitx: repository is locked by another repo-keeper operation")

const staleLock = time.Hour

// Lock takes a per-repository lock file inside the git dir and returns its release func.
func (g *Repo) Lock(ctx context.Context) (func(), error) {
	gd, err := g.line(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	p := filepath.Join(gd, "repo-keeper.lock")
	for attempt := 0; attempt < 2; attempt++ {
		// #nosec G304 -- path is inside the repo git dir
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // see #nosec above
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(p) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		st, serr := os.Stat(p)
		if serr == nil && time.Since(st.ModTime()) > staleLock { // crashed holder
			_ = os.Remove(p)
			continue
		}
		return nil, ErrLocked
	}
	return nil, ErrLocked
}
