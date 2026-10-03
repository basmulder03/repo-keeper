// SPDX-License-Identifier: Apache-2.0

// Package instance guarantees one repo-keeper daemon per user via an OS-level file lock.
package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrRunning means another daemon already holds the lock.
var ErrRunning = errors.New("instance: another repo-keeper daemon is already running")

// Lock is a held single-instance lock; the OS releases it if the process dies.
type Lock struct{ f *os.File }

// Acquire takes the lock at path (created with 0600, parent 0700).
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("instance: %w", err)
	}
	f, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	name := l.f.Name()
	_ = l.f.Close()
	_ = os.Remove(name) // best effort; the lock itself lived on the open handle
	l.f = nil
}

// Holder reports whether a daemon holds the lock at path and, when readable, its pid (0 = unknown).
func Holder(path string) (pid int, running bool) {
	f, err := lockFile(path)
	if err == nil {
		_ = f.Close() // nobody held it; closing drops our probe lock and leaves the file alone
		return 0, false
	}
	if !errors.Is(err, ErrRunning) {
		return 0, false
	}
	// #nosec G304 -- our own lock path
	b, rerr := os.ReadFile(path) //nolint:gosec // see #nosec above
	if rerr != nil {
		return 0, true // Windows denies reads while the lock is held
	}
	if n, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && n > 0 {
		pid = n
	}
	return pid, true
}
