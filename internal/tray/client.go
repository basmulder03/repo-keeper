// SPDX-License-Identifier: Apache-2.0

// Package tray is the logic of the optional tray helper: a thin client of the daemon's local API.
package tray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/basmulder03/repo-keeper/internal/control"
)

// ErrNotRunning means no reachable daemon was found.
var ErrNotRunning = errors.New("tray: daemon not running")

// Client talks to the daemon found through its runtime file; it re-reads the file on every call so a
// restarted daemon (new port, new control token) is picked up automatically.
type Client struct {
	RuntimeDir string
	HTTP       *http.Client
}

func (c *Client) runtime() (control.RuntimeFile, error) {
	var rf control.RuntimeFile
	// #nosec G304 -- our own runtime file
	data, err := os.ReadFile(filepath.Join(c.RuntimeDir, "ui.json")) //nolint:gosec // see #nosec above
	if err != nil {
		return rf, ErrNotRunning
	}
	if err := json.Unmarshal(data, &rf); err != nil || rf.Control == "" {
		return rf, ErrNotRunning
	}
	host, _, err := net.SplitHostPort(rf.Addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return rf, fmt.Errorf("tray: runtime file points at a non-loopback address %q", rf.Addr)
	}
	return rf, nil
}

func (c *Client) do(ctx context.Context, method, path string) (*http.Response, error) {
	rf, err := c.runtime()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	req, err := http.NewRequestWithContext(ctx, method, "http://"+rf.Addr+path, http.NoBody)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rf.Control)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := hc.Do(req) // loopback call to our own daemon
	if err != nil {
		cancel()
		return nil, ErrNotRunning
	}
	resp.Body = cancelBody{resp.Body, cancel}
	return resp, nil
}

type cancelBody struct {
	rc interface {
		Read([]byte) (int, error)
		Close() error
	}
	cancel context.CancelFunc
}

func (b cancelBody) Read(p []byte) (int, error) { return b.rc.Read(p) }
func (b cancelBody) Close() error               { b.cancel(); return b.rc.Close() }

// Status fetches the fleet summary.
func (c *Client) Status(ctx context.Context) (control.Status, error) {
	var st control.Status
	resp, err := c.do(ctx, http.MethodGet, "/api/status")
	if err != nil {
		return st, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("tray: daemon answered HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&st); err != nil {
		return st, fmt.Errorf("tray: bad status payload: %w", err)
	}
	return st, nil
}

// SyncAll queues every repository for an immediate sync.
func (c *Client) SyncAll(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPost, "/api/sync-all")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("tray: daemon answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// SetPaused pauses or resumes scheduled syncs.
func (c *Client) SetPaused(ctx context.Context, paused bool) error {
	path := "/api/resume"
	if paused {
		path = "/api/pause"
	}
	resp, err := c.do(ctx, http.MethodPost, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("tray: daemon answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// LoginURL returns a one-time sign-in link for the web UI.
func (c *Client) LoginURL(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/api/login-url")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tray: daemon answered HTTP %d", resp.StatusCode)
	}
	var out struct{ URL string }
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4096)).Decode(&out); err != nil || out.URL == "" {
		return "", errors.New("tray: unexpected login-url answer")
	}
	return out.URL, nil
}
