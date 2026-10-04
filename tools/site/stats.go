// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type coverage struct {
	Percent float64
	ByPkg   []pkgCoverage
}

type pkgCoverage struct {
	Pkg     string
	Percent float64
}

// parseCoverage reads a Go coverage profile ("file:start,end statements count") the way `go tool cover -func` totals it.
func parseCoverage(r *bufio.Scanner, module string) (coverage, error) {
	type acc struct{ total, covered int }
	pk := map[string]*acc{}
	var all acc
	for r.Scan() {
		line := r.Text()
		if strings.HasPrefix(line, "mode:") || strings.TrimSpace(line) == "" {
			continue
		}
		colon := strings.LastIndex(line, ":")
		fields := strings.Fields(line[colon+1:])
		if colon < 0 || len(fields) != 3 {
			return coverage{}, fmt.Errorf("unexpected coverage line %q", line)
		}
		stmts, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			return coverage{}, fmt.Errorf("unexpected coverage line %q", line)
		}
		pkg := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(line[:colon])), module+"/")
		if strings.HasSuffix(pkg, "test") { // gitxtest, providertest: test helpers, not product code
			continue
		}
		a := pk[pkg]
		if a == nil {
			a = &acc{}
			pk[pkg] = a
		}
		a.total += stmts
		all.total += stmts
		if count > 0 {
			a.covered += stmts
			all.covered += stmts
		}
	}
	if err := r.Err(); err != nil {
		return coverage{}, fmt.Errorf("unreadable coverage profile: %w", err)
	}
	if all.total == 0 {
		return coverage{}, errors.New("empty coverage profile")
	}
	c := coverage{Percent: 100 * float64(all.covered) / float64(all.total)}
	for p, a := range pk {
		if a.total > 0 {
			c.ByPkg = append(c.ByPkg, pkgCoverage{p, 100 * float64(a.covered) / float64(a.total)})
		}
	}
	sort.Slice(c.ByPkg, func(i, j int) bool { return c.ByPkg[i].Pkg < c.ByPkg[j].Pkg })
	return c, nil
}

type goMod struct {
	Module           string
	Direct, Indirect []string
}

func parseGoMod(data string) goMod {
	var m goMod
	in := false
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "module "):
			m.Module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		case line == "require (":
			in = true
		case line == ")":
			in = false
		case in || strings.HasPrefix(line, "require "):
			l := strings.TrimSpace(strings.TrimPrefix(line, "require "))
			if l == "" || strings.HasPrefix(l, "//") {
				continue
			}
			if strings.Contains(l, "// indirect") {
				m.Indirect = append(m.Indirect, strings.Fields(l)[0])
			} else {
				m.Direct = append(m.Direct, strings.Fields(l)[0]+" "+strings.Fields(l)[1])
			}
		}
	}
	return m
}

type codeStats struct{ Code, Test, Files, TestFuncs, Fuzz, Packages int }

func countCode(root string) (codeStats, error) {
	var s codeStats
	pkgs := map[string]bool{}
	for _, top := range []string{"cmd", "internal"} {
		if _, err := os.Stat(filepath.Join(root, top)); err != nil {
			continue // a tree without this directory simply has nothing to count
		}
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			// #nosec G304 G122 -- walking the repository we were pointed at, read-only
			f, err := os.Open(p) //nolint:gosec // see #nosec above
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			test := strings.HasSuffix(p, "_test.go")
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			lines := 0
			for sc.Scan() {
				lines++
				t := sc.Text()
				switch {
				case test && strings.HasPrefix(t, "func Test"):
					s.TestFuncs++
				case test && strings.HasPrefix(t, "func Fuzz"):
					s.Fuzz++
				}
			}
			s.Files++
			if test {
				s.Test += lines
			} else {
				s.Code += lines
				pkgs[filepath.Dir(p)] = true
			}
			return sc.Err()
		})
		if err != nil {
			return s, err
		}
	}
	s.Packages = len(pkgs)
	return s, nil
}

func providerPackages(root string) []string {
	entries, _ := os.ReadDir(filepath.Join(root, "internal", "provider"))
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "all" && e.Name() != "providertest" {
			out = append(out, e.Name())
		}
	}
	return out
}

func gitOut(root string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// #nosec G204 -- fixed git subcommands
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...) //nolint:gosec,forbidigo // sanctioned: build-time repository facts
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func genStats(b *build) (string, error) {
	row := func(k, v string) string {
		return "<tr><th scope=\"row\">" + html.EscapeString(k) + "</th><td>" + v + "</td></tr>\n"
	}
	gm, err := b.read("go.mod")
	if err != nil {
		return "", err
	}
	mod := parseGoMod(string(gm))
	cs, err := countCode(b.Repo)
	if err != nil {
		return "", err
	}
	var h strings.Builder
	h.WriteString("<p>Everything on this page is computed by the site build from the repository at commit <code>" + html.EscapeString(b.Commit) + "</code>; nothing is typed in by hand.</p>\n")
	h.WriteString("<h2 id=\"project\">Project</h2>\n<div class=\"table-wrap\"><table><tbody>\n")
	h.WriteString(row("Version", "<code>"+html.EscapeString(b.Version)+"</code>"))
	h.WriteString(row("Go source", fmt.Sprintf("%d lines in %d packages", cs.Code, cs.Packages)))
	h.WriteString(row("Tests", fmt.Sprintf("%d test functions, %d fuzz targets, %d lines of test code", cs.TestFuncs, cs.Fuzz, cs.Test)))
	h.WriteString(row("Providers", fmt.Sprintf("%d packages: %s", len(providerPackages(b.Repo)), html.EscapeString(strings.Join(providerPackages(b.Repo), ", ")))))
	h.WriteString(row("Direct dependencies", fmt.Sprintf("%d (and %d indirect)", len(mod.Direct), len(mod.Indirect))))
	if st, err := os.Stat(b.Bin); err == nil {
		h.WriteString(row("Binary size", human(st.Size())+" (this build, unstripped)"))
	}
	if n := gitOut(b.Repo, "rev-list", "--count", "HEAD"); n != "" {
		h.WriteString(row("Commits", html.EscapeString(n)))
	}
	if n := gitOut(b.Repo, "tag", "--list", "v*"); n != "" {
		h.WriteString(row("Tagged releases", strconv.Itoa(len(strings.Fields(n)))))
	}
	h.WriteString("</tbody></table></div>\n")

	h.WriteString("<h2 id=\"dependencies\">Direct dependencies</h2>\n<ul>\n")
	for _, d := range mod.Direct {
		h.WriteString("<li><code>" + html.EscapeString(d) + "</code></li>\n")
	}
	h.WriteString("</ul>\n")

	if b.Coverage != "" {
		f, err := os.Open(b.Coverage)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		cov, err := parseCoverage(bufio.NewScanner(f), mod.Module)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&h, "<h2 id=\"coverage\">Test coverage</h2>\n<p>Statement coverage over all packages: <strong>%.1f%%</strong> (test helper packages excluded).</p>\n", cov.Percent)
		h.WriteString("<div class=\"table-wrap\" tabindex=\"0\"><table><thead><tr><th>Package</th><th>Coverage</th></tr></thead><tbody>\n")
		for _, p := range cov.ByPkg {
			fmt.Fprintf(&h, "<tr><td><code>%s</code></td><td>%.1f%%</td></tr>\n", html.EscapeString(p.Pkg), p.Percent)
		}
		h.WriteString("</tbody></table></div>\n")
	}

	rs, err := b.loadReleases()
	if err != nil {
		return "", err
	}
	if len(rs) > 0 {
		var downloads int
		for _, r := range rs {
			for _, a := range r.Assets {
				downloads += a.Count
			}
		}
		fmt.Fprintf(&h, "<h2 id=\"releases\">Releases</h2>\n<p>%d published releases, latest <a href=\"%s\">%s</a> on %s; %d file downloads in total.</p>\n",
			len(rs), html.EscapeString(rs[0].URL), html.EscapeString(rs[0].Tag), rs[0].Published.Format("2006-01-02"), downloads)
	}
	return h.String(), nil
}
