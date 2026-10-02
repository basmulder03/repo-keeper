// SPDX-License-Identifier: Apache-2.0

// Package paths resolves per-user state locations.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// StateDir is the per-user state directory (audit journal, later the database).
func StateDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "repo-keeper"), nil
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "repo-keeper"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("paths: cannot determine home directory")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "repo-keeper"), nil
	}
	return filepath.Join(home, ".local", "state", "repo-keeper"), nil
}

// JournalPath is the default audit journal file.
func JournalPath() (string, error) {
	d, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "audit.jsonl"), nil
}
