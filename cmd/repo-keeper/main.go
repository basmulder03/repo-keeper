// SPDX-License-Identifier: Apache-2.0

// Command repo-keeper keeps local clones of your remote repositories in sync.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
)

// Set at build time via -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a := &app{out: os.Stdout, err: os.Stderr, newRunner: gitx.New, clock: clock.Real{}}
	os.Exit(a.run(ctx, os.Args[1:]))
}

// run dispatches subcommands and returns the process exit code.
func (a *app) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage(a.err)
		return 2
	}
	rest := args[1:]
	switch args[0] {
	case "sync":
		return a.cmdSync(ctx, rest)
	case "cleanup":
		return a.cmdCleanup(ctx, rest)
	case "restore":
		return a.cmdRestore(ctx, rest)
	case "audit":
		return a.cmdAudit(ctx, rest)
	case "doctor":
		return a.cmdDoctor(ctx, rest)
	case "version", "--version", "-v":
		a.printf("repo-keeper %s (%s)\n", version, commit)
		return 0
	case "help", "--help", "-h":
		usage(a.out)
		return 0
	default:
		_, _ = fmt.Fprintf(a.err, "repo-keeper: unknown command %q\n\n", args[0])
		usage(a.err)
		return 2
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: repo-keeper <command> [flags]

Commands:
  sync <repo>             fetch, fast-forward the default branch, optionally clean merged branches
  cleanup <repo>          evaluate/delete merged local branches (dry-run unless --cleanup=auto)
  restore <repo> <branch> bring back a branch deleted by cleanup (kept 30 days)
  audit                   show the journal of deletions, restores and blocked cleanups
  doctor                  check git version and state directory
  version                 print version

Run "repo-keeper <command> -h" for flags. Daemon, UI and provider discovery arrive in later milestones.
`)
}
