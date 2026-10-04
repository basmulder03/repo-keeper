// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// linker rewrites links found in the Markdown sources: links between documents become links between site pages,
// links to other repository files become GitHub links, and a link to a document that is not on the site is an error
// (so the documentation cannot rot unnoticed).
type linker struct {
	bySource map[string]string // repo-relative Markdown path -> page slug
	repoURL  string            // https://github.com/owner/repo
	branch   string
	problems *[]string
	from     string // repo-relative path of the document being rendered
}

func (l linker) rewrite(dest string) string {
	u, err := url.Parse(dest)
	if err != nil || u.Scheme != "" || u.Host != "" || dest == "" || strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "mailto:") {
		return dest
	}
	target := path.Clean(path.Join(path.Dir(l.from), u.Path))
	if strings.HasPrefix(target, "..") {
		*l.problems = append(*l.problems, fmt.Sprintf("%s: link %q leaves the repository", l.from, dest))
		return dest
	}
	if slug, ok := l.bySource[target]; ok {
		return slug + ".html" + fragment(u)
	}
	if strings.HasSuffix(target, ".md") {
		*l.problems = append(*l.problems, fmt.Sprintf("%s: link %q points to a Markdown file that is not published on the site", l.from, dest))
		return dest
	}
	kind := "blob"
	if strings.HasSuffix(u.Path, "/") || path.Ext(target) == "" {
		kind = "tree"
	}
	return l.repoURL + "/" + kind + "/" + l.branch + "/" + target
}

func fragment(u *url.URL) string {
	if u.Fragment == "" {
		return ""
	}
	return "#" + u.Fragment
}

type linkTransformer struct{ l *linker }

func (t linkTransformer) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if lk, ok := n.(*ast.Link); ok && entering {
			lk.Destination = []byte(t.l.rewrite(string(lk.Destination)))
		}
		return ast.WalkContinue, nil
	})
}

// markdown renders one document.
func markdown(src []byte, l *linker) (string, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList, extension.Linkify),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(linkTransformer{l}, 100)),
		),
		goldmark.WithRendererOptions(html.WithHardWraps()),
	)
	var out bytes.Buffer
	if err := md.Convert(src, &out); err != nil {
		return "", err
	}
	return wrapTables(out.String()), nil
}

// wrapTables lets wide tables scroll sideways instead of breaking the page layout on a phone.
func wrapTables(h string) string {
	h = strings.ReplaceAll(h, "<table>", `<div class="table-wrap" tabindex="0"><table>`)
	return strings.ReplaceAll(h, "</table>", "</table></div>")
}
