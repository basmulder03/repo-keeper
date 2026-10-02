// SPDX-License-Identifier: Apache-2.0

// Package askpass is the GIT_ASKPASS helper: it answers git's credential prompts for exactly one host.
package askpass

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Environment variables the daemon sets for a single git invocation.
const (
	EnvMarker = "REPO_KEEPER_ASKPASS" // "1" switches the binary into helper mode
	EnvHost   = "REPO_KEEPER_ASKPASS_HOST"
	EnvUser   = "REPO_KEEPER_ASKPASS_USER"
	// #nosec G101 -- an environment variable name, not a credential
	EnvSecret = "REPO_KEEPER_ASKPASS_SECRET" //nolint:gosec // see #nosec above
)

// Run answers one prompt (args[0]) and returns the exit code. It refuses prompts for any other host, so a
// redirect, submodule URL or insteadOf rewrite can never receive the token.
func Run(args []string, getenv func(string) string, out io.Writer) int {
	if len(args) == 0 {
		return 1
	}
	prompt := args[0]
	host := promptHost(prompt)
	if host == "" || !strings.EqualFold(host, getenv(EnvHost)) {
		return 1
	}
	switch {
	case strings.HasPrefix(prompt, "Username"):
		_, _ = fmt.Fprintln(out, getenv(EnvUser))
	case strings.HasPrefix(prompt, "Password"):
		_, _ = fmt.Fprintln(out, getenv(EnvSecret))
	default:
		return 1
	}
	return 0
}

// promptHost extracts host[:port] from prompts like "Password for 'https://user@host:8443': ".
func promptHost(prompt string) string {
	_, rest, ok := strings.Cut(prompt, "'")
	if !ok {
		return ""
	}
	raw, _, ok := strings.Cut(rest, "'")
	if !ok {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
