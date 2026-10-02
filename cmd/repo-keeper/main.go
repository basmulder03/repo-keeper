// SPDX-License-Identifier: Apache-2.0

// Command repo-keeper keeps local clones of your remote repositories in sync.
package main

import (
	"fmt"
	"io"
	"os"
)

// Set at build time via -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches subcommands and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		_, _ = fmt.Fprintf(stdout, "repo-keeper %s (%s)\n", version, commit)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "repo-keeper: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: repo-keeper <command>

Commands:
  version   print version
  help      show this help

More commands (daemon, ui, sync, ...) arrive in later milestones.
`)
}
