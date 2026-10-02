// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/paths"
	"github.com/basmulder03/repo-keeper/internal/secrets"
	"github.com/basmulder03/repo-keeper/internal/syncer"
)

// app carries injectable dependencies so commands are testable without touching the real HOME.
type app struct {
	out, err    io.Writer
	in          io.Reader
	secrets     secrets.Store
	newRunner   func() (*gitx.Runner, error)
	clock       clock.Clock
	openBrowser func(url string) error
	reexec      func() error // replaces the process after a requested restart (stubbed in tests)
}

const trashRetention = 30 * 24 * time.Hour

// policyFlags registers the flags shared by sync and cleanup.
type policyFlags struct {
	mode      string
	minAge    time.Duration
	protected string
	neverPush bool
	journal   string
}

func (p *policyFlags) register(fs *flag.FlagSet, defMode string) {
	fs.StringVar(&p.mode, "cleanup", defMode, "branch cleanup mode: off | dry-run | auto")
	fs.DurationVar(&p.minAge, "min-age", 7*24*time.Hour, "never delete branches whose tip is younger than this")
	fs.StringVar(&p.protected, "protected", strings.Join(cleanup.DefaultProtected, ","), "comma-separated protected branch globs")
	fs.BoolVar(&p.neverPush, "allow-never-pushed", false, "also delete merged branches that were never pushed")
	fs.StringVar(&p.journal, "journal", "", "audit journal file (default: per-user state dir)")
}

func (p *policyFlags) policy() (cleanup.Policy, error) {
	m := cleanup.Mode(p.mode)
	if m != cleanup.ModeOff && m != cleanup.ModeDryRun && m != cleanup.ModeAuto {
		return cleanup.Policy{}, fmt.Errorf("invalid --cleanup %q (want off, dry-run or auto)", p.mode)
	}
	var prot []string
	for _, s := range strings.Split(p.protected, ",") {
		if s = strings.TrimSpace(s); s != "" {
			prot = append(prot, s)
		}
	}
	return cleanup.Policy{Mode: m, Protected: prot, MinAge: p.minAge, AllowNeverPushed: p.neverPush}, nil
}

func (p *policyFlags) openJournal() (*audit.File, error) {
	path := p.journal
	if path == "" {
		var err error
		if path, err = paths.JournalPath(); err != nil {
			return nil, err
		}
	}
	return audit.OpenFile(path)
}

func (a *app) printf(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format, args...) }

// repoArg validates the single positional repository path.
func repoArg(fs *flag.FlagSet, want int) ([]string, error) {
	if fs.NArg() != want {
		return nil, fmt.Errorf("expected %d argument(s), got %d", want, fs.NArg())
	}
	args := fs.Args()
	abs, err := filepath.Abs(args[0])
	if err != nil {
		return nil, err
	}
	args[0] = abs
	return args, nil
}

func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.err)
	return fs
}

func (a *app) cmdSync(ctx context.Context, args []string) int {
	fs := a.newFlagSet("sync")
	var pf policyFlags
	pf.register(fs, "dry-run")
	defaultOnly := fs.Bool("default-only", false, "fetch only the default branch")
	remote := fs.String("remote", "origin", "remote name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pos, err := repoArg(fs, 1)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper sync [flags] <repo-path>:", err)
		return 2
	}
	pol, err := pf.policy()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 2
	}
	runner, err := a.newRunner()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	var j audit.Journal
	if pol.Mode == cleanup.ModeAuto {
		jf, err := pf.openJournal()
		if err != nil {
			_, _ = fmt.Fprintln(a.err, err)
			return 1
		}
		j = jf
	}
	res := syncer.Sync(ctx, syncer.Input{
		Git: runner.Repo(pos[0]), Path: pos[0], Remote: *remote, AllBranches: !*defaultOnly,
		Cleanup: pol, Journal: j, Clock: a.clock,
	})
	a.printResult(pos[0], res)
	if res.Status == syncer.Failed {
		return 1
	}
	return 0
}

func (a *app) printResult(path string, r syncer.Result) {
	a.printf("%s: %s", path, r.Status)
	if r.Reason != "" {
		a.printf(" (%s)", r.Reason)
	}
	a.printf("\n")
	if r.Err != nil {
		a.printf("  error: %v\n", r.Err)
	}
	if r.DefaultBranch != "" {
		a.printf("  default branch: %s", r.DefaultBranch)
		if r.DefaultChanged {
			a.printf(" (was %s)", r.PrevDefault)
		}
		a.printf("\n")
	}
	if r.FF != syncer.NotAttempted {
		a.printf("  fetched: %v, default branch: %s\n", r.Fetched, r.FF)
	}
	if r.Cleanup != nil {
		a.printReport(r.Cleanup)
	}
}

func (a *app) printReport(rep *cleanup.Report) {
	if rep.Blocked != "" {
		a.printf("  cleanup blocked: %s\n", rep.Blocked)
	}
	tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
	for _, it := range rep.Items {
		if it.Reason == cleanup.SkipDefault || it.Reason == cleanup.SkipCurrent {
			continue
		}
		extra := ""
		if it.Err != "" {
			extra = it.Err
		}
		_, _ = fmt.Fprintf(tw, "  branch\t%s\t%s\t%s\t%s\n", it.Branch, it.Outcome, it.Reason, extra)
	}
	_ = tw.Flush()
}

func (a *app) cmdCleanup(ctx context.Context, args []string) int {
	fs := a.newFlagSet("cleanup")
	var pf policyFlags
	pf.register(fs, "dry-run")
	remote := fs.String("remote", "origin", "remote name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pos, err := repoArg(fs, 1)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper cleanup [flags] <repo-path>:", err)
		return 2
	}
	pol, err := pf.policy()
	if err != nil || pol.Mode == cleanup.ModeOff {
		_, _ = fmt.Fprintln(a.err, "cleanup needs --cleanup=dry-run or auto", err)
		return 2
	}
	runner, err := a.newRunner()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	g := runner.Repo(pos[0])
	def, err := g.OriginHead(ctx, *remote)
	if err != nil || def == "" {
		_, _ = fmt.Fprintf(a.err, "cannot determine default branch (run `repo-keeper sync` first): %v\n", err)
		return 1
	}
	if err := g.CheckSafeConfig(ctx); err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	jf, err := pf.openJournal()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	unlock, err := g.Lock(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	defer unlock()
	rep, err := cleanup.Run(ctx, cleanup.Input{
		Git: g, Repo: pos[0], Remote: *remote, DefaultBranch: def, Policy: pol, Journal: jf, Clock: a.clock,
	})
	a.printReport(&rep)
	if _, perr := cleanup.PurgeTrash(ctx, g, jf, pos[0], trashRetention, a.clock.Now()); perr != nil {
		_, _ = fmt.Fprintln(a.err, "trash purge:", perr)
	}
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	return 0
}

func (a *app) cmdRestore(ctx context.Context, args []string) int {
	fs := a.newFlagSet("restore")
	var pf policyFlags
	pf.register(fs, "off")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pos, err := repoArg(fs, 2)
	if err != nil {
		_, _ = fmt.Fprintln(a.err, "usage: repo-keeper restore [flags] <repo-path> <branch>:", err)
		return 2
	}
	runner, err := a.newRunner()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	jf, err := pf.openJournal()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	t, err := cleanup.Restore(ctx, runner.Repo(pos[0]), jf, pos[0], pos[1], a.clock.Now())
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	a.printf("restored %s at %s (deleted %s)\n", pos[1], t.SHA[:12], t.DeletedAt.Format(time.RFC3339))
	return 0
}

func (a *app) cmdAudit(_ context.Context, args []string) int {
	fs := a.newFlagSet("audit")
	var pf policyFlags
	pf.register(fs, "off")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	jf, err := pf.openJournal()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	entries, err := jf.ReadAll()
	if err != nil {
		_, _ = fmt.Fprintln(a.err, err)
		return 1
	}
	tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
	for _, e := range entries {
		sha := e.SHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Format(time.RFC3339), e.Action, e.Repo, e.Branch, sha, e.Reason)
	}
	_ = tw.Flush()
	return 0
}

func (a *app) cmdDoctor(ctx context.Context, _ []string) int {
	code := 0
	runner, err := a.newRunner()
	if err != nil {
		a.printf("FAIL git: %v\n", err)
		return 1
	}
	v, _ := runner.Version(ctx)
	a.printf("ok   git %s at %s (minimum %s)\n", v, runner.Bin, gitx.MinVersion)
	dir, err := paths.StateDir()
	if err == nil {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			var f *os.File
			if f, err = os.CreateTemp(dir, ".doctor-*"); err == nil {
				_ = f.Close()
				_ = os.Remove(f.Name())
			}
		}
	}
	if err != nil {
		a.printf("FAIL state dir %s: %v\n", dir, err)
		code = 1
	} else {
		a.printf("ok   state dir %s writable\n", dir)
	}
	return code
}
