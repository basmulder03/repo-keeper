// SPDX-License-Identifier: Apache-2.0

package update

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const applyImport = "github.com/basmulder03/repo-keeper/internal/update/apply"

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "_site" || d.Name() == "tools") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestApplyIsReachableOnlyFromTheExplicitUpdateCommand is ADR-0021 as a build failure: the code that replaces the
// running program may be imported by cmd/repo-keeper/update.go and by its own tests, and by nothing else. In particular
// the daemon, the scheduler, the web interface and the tray can never apply an update, however the code evolves.
func TestApplyIsReachableOnlyFromTheExplicitUpdateCommand(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var importers []string
	for _, p := range goFiles(t, root) {
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != applyImport {
				continue
			}
			rel, _ := filepath.Rel(root, p)
			importers = append(importers, filepath.ToSlash(rel))
		}
	}
	allowed := func(rel string) bool {
		return rel == "cmd/repo-keeper/update.go" || rel == "cmd/repo-keeper/update_test.go" || strings.HasPrefix(rel, "internal/update/apply/")
	}
	for _, rel := range importers {
		if !allowed(rel) {
			t.Errorf("%s imports the apply package: only the explicit `update` command may (ADR-0021)", rel)
		}
	}
	if len(importers) == 0 {
		t.Fatal("found no importer at all: the scan itself is broken")
	}
	var sawCmd bool
	for _, rel := range importers {
		sawCmd = sawCmd || rel == "cmd/repo-keeper/update.go"
	}
	if !sawCmd {
		t.Fatal("cmd/repo-keeper/update.go must be the importer")
	}
}

// TestUpdateCommandHasExactlyOneEntryPoint: cmdUpdate is called from the "update" case of the command switch and from
// nowhere else, and the helpers that install or roll back are only called from update.go.
func TestUpdateCommandHasExactlyOneEntryPoint(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "cmd", "repo-keeper")
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files[n] = f
		}
	}
	guarded := map[string]bool{"cmdUpdate": true, "updateCheckOrApply": true, "applyRelease": true, "updateRollback": true}
	calls := map[string][]string{}
	for base, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && guarded[sel.Sel.Name] {
				calls[sel.Sel.Name] = append(calls[sel.Sel.Name], base)
			}
			return true
		})
	}
	if got := calls["cmdUpdate"]; len(got) != 1 || got[0] != "main.go" {
		t.Errorf("cmdUpdate must be called exactly once, from main.go's command switch: %v", got)
	}
	for _, name := range []string{"updateCheckOrApply", "applyRelease", "updateRollback"} {
		for _, file := range calls[name] {
			if file != "update.go" {
				t.Errorf("%s is called from %s: installing may only be reached through cmdUpdate in update.go", name, file)
			}
		}
	}
	// the call in main.go must sit under `case "update":`
	data := parseSwitch(t, filepath.Join(dir, "main.go"))
	if !data {
		t.Error(`main.go must call cmdUpdate only under case "update"`)
	}
}

func parseSwitch(t *testing.T, path string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	ast.Inspect(f, func(n ast.Node) bool {
		cc, isCase := n.(*ast.CaseClause)
		if !isCase {
			return true
		}
		for _, e := range cc.List {
			if lit, isLit := e.(*ast.BasicLit); isLit && lit.Value == `"update"` {
				for _, s := range cc.Body {
					ast.Inspect(s, func(m ast.Node) bool {
						if c, isCall := m.(*ast.CallExpr); isCall {
							if sel, isSel := c.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == "cmdUpdate" {
								ok = true
							}
						}
						return true
					})
				}
			}
		}
		return true
	})
	return ok
}

// TestNothingAutomatedMentionsInstalling guards the daemon-side files by name: they may read and write the update
// state, never touch the apply package or run anything.
func TestDaemonUpdateFileNeverExecutesOrInstalls(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"internal/daemon/update.go", "internal/update/release.go", "internal/update/state.go", "internal/update/channel.go", "internal/update/version.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, rel), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			switch strings.Trim(imp.Path.Value, `"`) {
			case "os/exec", applyImport, "archive/tar", "compress/gzip", "syscall":
				t.Errorf("%s imports %s: the notification side must not be able to run or unpack anything", rel, imp.Path.Value)
			}
		}
	}
}
