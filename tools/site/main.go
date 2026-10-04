// SPDX-License-Identifier: Apache-2.0

// Command site builds the static documentation site from the repository: Markdown documents plus pages generated
// from the built binary, the source tree, the test coverage profile and the GitHub releases.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var b build
	var date string
	flag.StringVar(&b.Repo, "repo", "../..", "repository root")
	flag.StringVar(&b.Out, "out", "_site", "output directory")
	flag.StringVar(&b.Bin, "bin", "", "path to the built repo-keeper binary (required)")
	flag.StringVar(&b.Coverage, "coverage", "", "Go coverage profile for the statistics page (optional)")
	flag.StringVar(&b.Releases, "releases", "", "GitHub releases JSON for downloads and statistics (optional)")
	flag.StringVar(&b.RepoURL, "repo-url", "https://github.com/basmulder03/repo-keeper", "repository URL for source links")
	flag.StringVar(&b.Branch, "branch", "main", "branch used in source links")
	flag.StringVar(&date, "date", "", "build date, YYYY-MM-DD (default today, UTC)")
	flag.Parse()
	if b.Bin == "" {
		fatal("-bin is required: the CLI reference and starter configuration are generated from the real binary")
	}
	abs := func(p *string) {
		if *p != "" {
			if a, err := filepath.Abs(*p); err == nil {
				*p = a
			}
		}
	}
	abs(&b.Repo)
	abs(&b.Bin)
	abs(&b.Coverage)
	abs(&b.Releases)
	v, err := os.ReadFile(filepath.Join(b.Repo, "VERSION"))
	if err != nil {
		fatal("reading VERSION: " + err.Error())
	}
	b.Version = strings.TrimSpace(string(v))
	b.Commit = gitOut(b.Repo, "rev-parse", "--short", "HEAD")
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	b.Date = time.Now().UTC()
	if date != "" {
		if b.Date, err = time.Parse("2006-01-02", date); err != nil {
			fatal("bad -date: " + err.Error())
		}
	}
	if err := b.generate(); err != nil {
		fatal(err.Error())
	}
	_, _ = fmt.Fprintf(os.Stdout, "site written to %s (%d pages)\n", b.Out, len(b.pages)+1)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "site:", msg)
	os.Exit(1)
}
