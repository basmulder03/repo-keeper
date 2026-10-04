// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
	"github.com/basmulder03/repo-keeper/internal/update"
	"github.com/basmulder03/repo-keeper/internal/update/apply"
)

// This file is the ONLY place that imports internal/update/apply: replacing the running program happens here, behind
// an explicit command and an interactive confirmation, and nowhere else (ADR-0021; an architecture test enforces it).

// updateEnv holds what the update command depends on, so every refusal path can be tested.
type updateEnv struct {
	exe         func() (string, error)
	probe       update.Probe
	source      func() (apply.Source, error)
	interactive func() bool
}

func (e updateEnv) executable() (string, error) {
	if e.exe != nil {
		return e.exe()
	}
	return os.Executable()
}

func (e updateEnv) isInteractive() bool {
	if e.interactive != nil {
		return e.interactive()
	}
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (e updateEnv) newSource() (apply.Source, error) {
	if e.source != nil {
		return e.source()
	}
	hc, err := httpx.New(httpx.Config{
		UserAgent: "repo-keeper/" + version + " (+https://github.com/basmulder03/repo-keeper)",
		Limiter:   ratelimit.New(ratelimit.Config{}, clock.Real{}, nil),
	})
	if err != nil {
		return apply.Source{}, err
	}
	return apply.Source{Checker: update.Checker{HTTP: hc}, Verifier: apply.CosignCLI{}, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}, nil
}

func (a *app) cmdUpdate(ctx context.Context, args []string) int {
	fs := a.newFlagSet("update")
	check := fs.Bool("check", false, "show whether a newer release exists and the evidence to verify it (the default)")
	doApply := fs.Bool("apply", false, "download, verify and install it over this binary (asks first; tarball installs only)")
	rollback := fs.Bool("rollback", false, "put back the version that was kept before the last update")
	target := fs.String("version", "", "use exactly this release instead of the newest (with --apply this also allows a downgrade)")
	if fs.Parse(args) != nil {
		return 2
	}
	modes := 0
	for _, m := range []bool{*check, *doApply, *rollback} {
		if m {
			modes++
		}
	}
	if modes > 1 || fs.NArg() != 0 || (*rollback && *target != "") {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper update [--check | --apply [--version X] | --rollback]")
		return 2
	}
	exe, err := a.upd.executable()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "cannot tell where this program is installed:", err)
		return 1
	}
	ch := update.Detect(exe, a.upd.probe)
	if *rollback {
		return a.updateRollback(ch)
	}
	return a.updateCheckOrApply(ctx, ch, *doApply, *target)
}

func (a *app) updateCheckOrApply(ctx context.Context, ch update.Channel, doApply bool, want string) int {
	src, err := a.upd.newSource()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	cur, curErr := update.ParseVersion(version)
	if doApply && curErr != nil {
		_, _ = fmt.Fprintf(a.err, "this is a development build (%s): there is no release version to update from\n", version)
		return 1
	}
	if doApply && !ch.SelfUpdatable() {
		a.refuseManaged(ch)
		return 1
	}
	rs, err := src.Releases(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "could not read the releases from GitHub:", err)
		return 1
	}
	var rel update.Release
	explicit := want != ""
	if explicit {
		wv, perr := update.ParseVersion(want)
		if perr != nil {
			_, _ = fmt.Fprintf(a.err, "--version %q is not a release version like 0.1.0-beta.5\n", want)
			return 2
		}
		var ok bool
		if rel, ok = update.Find(rs, wv); !ok {
			_, _ = fmt.Fprintf(a.err, "no published release %s (drafts are never offered)\n", wv)
			return 1
		}
	} else {
		var nerr error
		if rel, _, nerr = update.Newest(rs, cur); nerr != nil {
			a.printf("no published release to update to (running %s)\n", version)
			return 0
		}
	}
	rv, _ := rel.Version()
	switch {
	case curErr == nil && rv.Compare(cur) == 0:
		a.printf("up to date: %s is the release you are running\n", rv)
		if !explicit {
			return 0
		}
	case curErr == nil && rv.Compare(cur) < 0 && !explicit:
		a.printf("up to date: %s is newer than the newest published release %s\n", cur, rv)
		return 0
	}
	ev, err := src.Gather(ctx, rel)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "could not collect the release evidence:", err)
		return 1
	}
	a.printEvidence(ev, ch, curErr == nil && rv.Compare(cur) < 0)
	if !doApply {
		if !ch.SelfUpdatable() {
			a.printf("\nThis install is managed by %s; update it with:\n  %s\n", ch.Kind, ch.Command)
		} else {
			a.printf("\nTo install it: repo-keeper update --apply%s\n", map[bool]string{true: " --version " + rv.String(), false: ""}[explicit])
		}
		return 0
	}
	return a.applyRelease(ctx, src, ev, ch, cur, rv)
}

func (a *app) refuseManaged(ch update.Channel) {
	_, _ = fmt.Fprintf(a.err, "this repo-keeper is managed by %s (%s), which must do the updating.\nUpdate it with:\n  %s\n", ch.Kind, ch.Exe, ch.Command)
}

func (a *app) applyRelease(ctx context.Context, src apply.Source, ev apply.Evidence, ch update.Channel, cur, target update.Version) int {
	if !ev.Signature.Verified {
		_, _ = fmt.Fprintf(a.err, "\nrefusing to install: the release signature is %s.\nInstall cosign (https://docs.sigstore.dev/cosign/system_config/installation/) or download and verify the release yourself with the commands above.\n", ev.Signature.Detail)
		return 1
	}
	if !a.upd.isInteractive() {
		_, _ = fmt.Fprintln(a.err, "\nupdate --apply asks for your confirmation and needs a terminal; run it yourself in one. It can never run unattended.")
		return 1
	}
	verb := "Install"
	if target.Compare(cur) < 0 {
		verb = "DOWNGRADE to"
	}
	a.printf("\n%s %s over %s at %s? [y/N] ", verb, target, cur, ch.Exe)
	answer, err := readLine(a.in)
	if err != nil || (strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes") {
		a.printf("\nnot installed (the default is No)\n")
		return 1
	}
	a.printf("downloading %s ...\n", ev.ArchiveName)
	archive, err := src.Fetch(ctx, &ev)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "refusing to install:", err)
		return 1
	}
	a.printf("sha256 computed %s\n       expected %s   match\n", ev.Computed, ev.ExpectedSHA)
	res, err := apply.Install(archive, ch.Exe)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "not installed:", err)
		return 1
	}
	a.printf("\ninstalled %s: %s\nthe previous version is kept (%s); undo with: repo-keeper update --rollback\n", target, strings.Join(res.Replaced, ", "), strings.Join(res.Previous, ", "))
	a.printf("A running daemon keeps the old version until it is restarted; restart it when you are ready:\n  repo-keeper restart        # or: systemctl --user restart repo-keeper\n")
	return 0
}

func (a *app) updateRollback(ch update.Channel) int {
	if !ch.SelfUpdatable() {
		a.refuseManaged(ch)
		return 1
	}
	if !a.upd.isInteractive() {
		_, _ = fmt.Fprintln(a.err, "update --rollback asks for your confirmation and needs a terminal; run it yourself in one.")
		return 1
	}
	prev := ch.Exe + apply.PreviousSuffix
	if _, err := os.Stat(prev); err != nil {
		_, _ = fmt.Fprintf(a.err, "there is no previous version kept next to %s (nothing was updated here)\n", ch.Exe)
		return 1
	}
	a.printf("Put back the previous version from %s over %s? [y/N] ", filepath.Base(prev), ch.Exe)
	answer, err := readLine(a.in)
	if err != nil || (strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes") {
		a.printf("\nnot changed (the default is No)\n")
		return 1
	}
	res, err := apply.Rollback(ch.Exe)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "not rolled back:", err)
		return 1
	}
	a.printf("\nrestored: %s\n(rolling back again returns to the version you just left)\nRestart a running daemon to use it: repo-keeper restart\n", strings.Join(res.Replaced, ", "))
	return 0
}

// printEvidence shows what a person needs to judge the release without trusting this program (ADR-0021).
func (a *app) printEvidence(ev apply.Evidence, ch update.Channel, downgrade bool) {
	cur := version
	a.printf("installed:  %s   (%s install at %s)\n", cur, ch.Kind, ch.Exe)
	flags := ""
	if ev.Release.Prerelease {
		flags += "  [pre-release]"
	}
	if ev.Release.Urgent() {
		flags += "  [SECURITY FIX]"
	}
	if downgrade {
		flags += "  [OLDER THAN WHAT YOU RUN]"
	}
	a.printf("target:     %s   published %s%s\n", ev.Version, ev.Release.Published.Format("2006-01-02"), flags)
	a.printf("release:    %s\n", ev.Release.URL)
	if lines := noteLines(ev.Release.Body, 12); len(lines) > 0 {
		a.printf("notes:\n")
		for _, l := range lines {
			a.printf("  %s\n", l)
		}
	}
	a.printf("asset:      %s\n", ev.Archive.URL)
	a.printf("sha256:     expected %s (from %s)\n", ev.ExpectedSHA, ev.ChecksumsAt)
	if ev.Signature.Verified {
		a.printf("signature:  verified (cosign, against the identity and issuer below)\n")
	} else {
		a.printf("signature:  %s\n", ev.Signature.Detail)
	}
	a.printf("  identity: %s\n  issuer:   %s\n", ev.Identity, ev.Issuer)
	switch {
	case ev.Commit != "" && ev.RunURL != "":
		a.printf("source:     tag %s is commit %s, built by %s\n", ev.Tag, ev.Commit, ev.RunURL)
	case ev.Commit != "":
		a.printf("source:     tag %s is commit %s (workflow run not found)\n", ev.Tag, ev.Commit)
	default:
		a.printf("source:     tag %s (commit lookup unavailable)\n", ev.Tag)
	}
	a.printf("\nVerify it yourself, independently of this program:\n")
	a.printf("  curl -fsSLO %s -O %s -O %s -O %s\n", ev.Archive.URL, ev.ChecksumsAt, ev.ChecksumsAt+".sig", ev.ChecksumsAt+".pem")
	a.printf("  cosign verify-blob checksums.txt --certificate checksums.txt.pem --signature checksums.txt.sig \\\n    --certificate-identity %s \\\n    --certificate-oidc-issuer %s\n", ev.Identity, ev.Issuer)
	a.printf("  sha256sum --check --ignore-missing checksums.txt\n  gh attestation verify %s --repo %s\n", ev.ArchiveName, ev.Repo)
}

// noteLines returns the first non-empty lines of the release notes, trimmed.
func noteLines(body string, max int) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r", ""), "\n") {
		if l = strings.TrimRight(l, " \t"); strings.TrimSpace(l) == "" {
			continue
		}
		if len(out) == max {
			out = append(out, "... (full notes at the release page)")
			break
		}
		if r := []rune(l); len(r) > 110 {
			l = string(r[:109]) + "…"
		}
		out = append(out, l)
	}
	return out
}
