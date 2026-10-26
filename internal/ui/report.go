// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/obs"
	"github.com/basmulder03/repo-keeper/internal/store"
)

const (
	issueNewURL  = "https://github.com/basmulder03/repo-keeper/issues/new"
	reportMaxURL = 7000 // GitHub rejects links much beyond 8 KB
)

var (
	urlRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	scpRe = regexp.MustCompile(`[\w.-]+@[\w.-]+:[^\s'"]+`)
)

// redactReport hides hosts, users, repository names and the home directory; the user reviews the rest before sending.
func redactReport(s string) string {
	s = obs.Scrub(s)
	s = urlRe.ReplaceAllStringFunc(s, func(m string) string { return m[:strings.Index(m, "://")] + "://<redacted>" })
	s = scpRe.ReplaceAllString(s, "<user>@<host>:<path>")
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// reportURL builds a bug-report link prefilled from the debug data; it is only a link, the user submits it on github.com.
func reportURL(info Info, cmds []gitx.CommandRecord, events []store.Event) string {
	ssh := "not found"
	if p, err := exec.LookPath("ssh"); err == nil {
		ssh = redactReport(p)
	}
	env := fmt.Sprintf("%s/%s, git %s, ssh %s", runtime.GOOS, runtime.GOARCH, info.GitVersion, ssh)

	var b strings.Builder
	b.WriteString("Environment (names only): ")
	for _, k := range []string{"GIT_SSH", "GIT_SSH_COMMAND", "SSH_AUTH_SOCK"} {
		_, set := os.LookupEnv(k)
		fmt.Fprintf(&b, "%s=%s ", k, map[bool]string{true: "set", false: "unset"}[set])
	}
	b.WriteString("\n")
	for _, c := range cmds { // newest first
		if c.Exit != 0 {
			fmt.Fprintf(&b, "\nFailed git command:\n```\ngit %s\nexit %d: %s\n```\n", redactReport(strings.Join(c.Args, " ")), c.Exit, redactReport(c.Err))
			break
		}
	}
	n := 0
	for _, e := range events {
		if e.Level != "info" && n < 5 {
			fmt.Fprintf(&b, "- %s %s: %s\n", e.Level, e.Code, redactReport(e.Message))
			n++
		}
	}
	what, r := b.String(), []rune(b.String())
	for {
		q := url.Values{"template": {"bug_report.yml"}, "version": {info.Version}, "env": {env}, "what": {what}}
		if link := issueNewURL + "?" + q.Encode(); len(link) <= reportMaxURL {
			return link
		}
		r = r[:len(r)*3/4]
		what = string(r) + "\n(truncated)"
	}
}
