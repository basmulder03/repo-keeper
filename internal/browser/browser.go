// SPDX-License-Identifier: Apache-2.0

// Package browser opens a local URL in the user's default browser.
package browser

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

// Open launches the default browser on a loopback http URL; anything else is refused so this can never be
// turned into "open an arbitrary URL/program".
func Open(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return errors.New("browser: only http loopback URLs may be opened")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return errors.New("browser: only http loopback URLs may be opened")
	}
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{rawURL}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		name, args = "xdg-open", []string{rawURL}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 -- fixed program names; the URL was validated as http loopback above
	return exec.CommandContext(ctx, name, args...).Run() //nolint:forbidigo,gosec // sanctioned: launching the user's browser
}
