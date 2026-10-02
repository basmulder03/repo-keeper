// SPDX-License-Identifier: Apache-2.0

// Package audit is the append-only journal of every local mutation repo-keeper performs.
package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Action is what happened.
type Action string

// Journal actions.
const (
	Deleted      Action = "deleted"
	DeleteFailed Action = "delete_failed"
	Restored     Action = "restored"
	Blocked      Action = "blocked"
	TrashPurged  Action = "trash_purged"
)

// Entry is one journal line.
type Entry struct {
	Time   time.Time `json:"time"`
	Repo   string    `json:"repo"`
	Branch string    `json:"branch,omitempty"`
	SHA    string    `json:"sha,omitempty"`
	Action Action    `json:"action"`
	Mode   string    `json:"mode,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Journal records entries; implementations must be safe for concurrent use.
type Journal interface {
	Append(ctx context.Context, e Entry) error
}

// File is a JSONL journal on disk (0600), fsynced per entry so a crash never loses an acknowledged line.
type File struct {
	mu   sync.Mutex
	path string
}

// OpenFile prepares the journal at path, creating parent directories.
func OpenFile(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	return &File{path: path}, nil
}

// Append implements Journal.
func (f *File) Append(_ context.Context, e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path is our own state file
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if _, err := fh.Write(append(b, '\n')); err != nil {
		_ = fh.Close()
		return fmt.Errorf("audit: %w", err)
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		return fmt.Errorf("audit: %w", err)
	}
	return fh.Close()
}

// ReadAll returns every entry in file order; malformed lines are skipped, not fatal.
func (f *File) ReadAll() ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, err := os.Open(f.path) //nolint:gosec // our own state file
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	var res []Entry
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			res = append(res, e)
		}
	}
	return res, sc.Err()
}

// Mem is an in-memory Journal for tests.
type Mem struct {
	mu      sync.Mutex
	Entries []Entry
	// Err, when set, makes Append fail (to test fail-closed behaviour).
	Err error
}

// Append implements Journal.
func (m *Mem) Append(_ context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	m.Entries = append(m.Entries, e)
	return nil
}
