// SPDX-License-Identifier: Apache-2.0

package main

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

	"github.com/basmulder03/repo-keeper/internal/paths"
	"github.com/basmulder03/repo-keeper/internal/ui"
)

// cmdUI asks the running daemon for a one-time sign-in link and opens it in the default browser.
func (a *app) cmdUI(ctx context.Context, args []string) int {
	fs := a.newFlagSet("ui")
	printOnly := fs.Bool("print-url", false, "print the sign-in URL instead of opening a browser (SSH port-forward: ssh -L PORT:127.0.0.1:PORT)")
	runtimeDir := fs.String("runtime-dir", "", "directory holding ui.json (default: per-user runtime dir)")
	if fs.Parse(args) != nil {
		return 2
	}
	dir := *runtimeDir
	if dir == "" {
		var err error
		if dir, err = paths.RuntimeDir(); err != nil {
			_, _ = fmt.Fprintln(a.err, err)
			return 1
		}
	}
	// #nosec G304 -- our own runtime file
	data, err := os.ReadFile(filepath.Join(dir, "ui.json")) //nolint:gosec // see #nosec above
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "the daemon is not running (no ui.json). Start it with `repo-keeper daemon` or `systemctl --user start repo-keeper`.")
		return 1
	}
	var rf ui.RuntimeFile
	if err := json.Unmarshal(data, &rf); err != nil || rf.Control == "" {
		_, _ = fmt.Fprintln(a.err, "ui.json is unreadable; restart the daemon")
		return 1
	}
	if host, _, err := net.SplitHostPort(rf.Addr); err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		_, _ = fmt.Fprintf(a.err, "ui.json points at a non-loopback address %q; refusing\n", rf.Addr)
		return 1
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, "http://"+rf.Addr+"/api/login-url", http.NoBody)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+rf.Control)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req) // loopback control call to our own daemon, not a provider
	if err != nil {
		_, _ = fmt.Fprintf(a.err, "the daemon does not answer on %s (stale ui.json?). Is it running?\n", rf.Addr)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(a.err, "the daemon refused the request (HTTP %d); restart it and retry\n", resp.StatusCode)
		return 1
	}
	var out struct{ URL string }
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4096)).Decode(&out); err != nil || out.URL == "" {
		_, _ = fmt.Fprintln(a.err, "unexpected answer from the daemon")
		return 1
	}
	if *printOnly {
		a.printf("%s\n", out.URL)
		return 0
	}
	if err := a.openBrowser(out.URL); err != nil {
		if !errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintf(a.err, "could not open a browser (%v). Open this link within 60 seconds:\n", err)
		}
		a.printf("%s\n", out.URL)
		return 0
	}
	a.printf("Opened the repo-keeper interface in your browser (%s).\n", rf.Addr)
	return 0
}
