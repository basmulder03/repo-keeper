// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed templates/*.html assets/*
var assets embed.FS

// build holds everything a page generator may need.
type build struct {
	Repo      string // repository root
	Out       string // output directory
	Bin       string // built repo-keeper binary
	Coverage  string // optional coverage profile
	Releases  string // optional GitHub releases JSON
	RepoURL   string
	Branch    string
	Version   string
	Commit    string
	Date      time.Time
	pages     []page
	problems  []string
	bySource  map[string]string
	layout    *template.Template
	rendering string
}

func (b *build) read(rel string) ([]byte, error) {
	// #nosec G304 -- repo-relative documentation paths fixed by pageList
	return os.ReadFile(filepath.Join(b.Repo, rel)) //nolint:gosec // see #nosec above
}

type pageData struct {
	Title, Summary, Section, Slug  string
	Content                        template.HTML
	ShowTitle                      bool // generated pages have no Markdown H1 of their own
	Nav                            []navSection
	Version, Commit, Date, RepoURL string
	Edit                           string // link to the Markdown source on GitHub, if any
	Cards                          []card // home page only
}

type navSection struct {
	Name  string
	Pages []navItem
}

type navItem struct {
	Slug, Title string
	Current     bool
}

type card struct{ Slug, Title, Summary string }

func (b *build) generate() error {
	b.pages = pageList()
	b.bySource = map[string]string{}
	for _, p := range b.pages {
		if p.Source != "" {
			b.bySource[p.Source] = p.Slug
		}
	}
	var err error
	if b.layout, err = template.ParseFS(assets, "templates/layout.html"); err != nil {
		return err
	}
	if err := os.MkdirAll(b.Out, 0o750); err != nil {
		return err
	}
	for _, p := range b.pages {
		body, err := b.content(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Slug, err)
		}
		if err := b.write(p, body); err != nil {
			return err
		}
	}
	if err := b.copyAssets(); err != nil {
		return err
	}
	if err := b.notFound(); err != nil {
		return err
	}
	b.checkInternalLinks()
	if len(b.problems) > 0 {
		return errors.New("documentation problems:\n  " + strings.Join(b.problems, "\n  "))
	}
	return os.WriteFile(filepath.Join(b.Out, ".nojekyll"), nil, 0o600)
}

// content renders a page: Markdown source, a generated body, or a Markdown source followed by a generated section.
func (b *build) content(p page) (string, error) {
	var out strings.Builder
	if p.Source != "" {
		src, err := b.read(p.Source)
		if err != nil {
			return "", err
		}
		l := &linker{bySource: b.bySource, repoURL: b.RepoURL, branch: b.Branch, problems: &b.problems, from: p.Source}
		h, err := markdown(src, l)
		if err != nil {
			return "", err
		}
		out.WriteString(h)
	}
	if p.gen != nil {
		b.rendering = p.Slug
		h, err := p.gen(b)
		if err != nil {
			return "", err
		}
		out.WriteString(h)
	}
	return out.String(), nil
}

func (b *build) write(p page, body string) error {
	d := pageData{
		Title: p.Title, Summary: p.Summary, Section: p.Section, Slug: p.Slug, Content: template.HTML(body), ShowTitle: p.Source == "", // #nosec G203 -- our own rendered repository Markdown and generated HTML
		Version: b.Version, Commit: b.Commit, Date: b.Date.Format("2006-01-02"), RepoURL: b.RepoURL, Nav: b.nav(p.Slug),
	}
	if p.Source != "" {
		d.Edit = b.RepoURL + "/blob/" + b.Branch + "/" + p.Source
	}
	if p.Slug == "index" {
		for _, q := range b.pages {
			if q.Slug != "index" && q.Slug != "docs-index" {
				d.Cards = append(d.Cards, card{q.Slug, q.Title, q.Summary})
			}
		}
	}
	var buf bytes.Buffer
	if err := b.layout.Execute(&buf, d); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.Out, p.Slug+".html"), buf.Bytes(), 0o600)
}

func (b *build) nav(current string) []navSection {
	var out []navSection
	for _, s := range sections {
		ns := navSection{Name: s}
		for _, p := range b.pages {
			if p.Section == s && p.Slug != "docs-index" {
				ns.Pages = append(ns.Pages, navItem{p.Slug, p.Title, p.Slug == current})
			}
		}
		out = append(out, ns)
	}
	return out
}

func (b *build) copyAssets() error {
	return fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := assets.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(b.Out, filepath.Base(p)), data, 0o600)
	})
}

func (b *build) notFound() error {
	p := page{Slug: "404", Title: "Page not found", Section: "Start"}
	return b.write(p, `<p>That page does not exist. Try the <a href="index.html">overview</a> or pick a topic in the menu.</p>`)
}

// checkInternalLinks verifies every relative .html link in the output points at a page that was written.
func (b *build) checkInternalLinks() {
	files, _ := filepath.Glob(filepath.Join(b.Out, "*.html"))
	for _, f := range files {
		// #nosec G304 -- our own output directory
		data, err := os.ReadFile(f) //nolint:gosec // see #nosec above
		if err != nil {
			continue
		}
		for _, href := range hrefs(string(data)) {
			if strings.Contains(href, "://") || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
				continue
			}
			target := strings.SplitN(strings.SplitN(href, "#", 2)[0], "?", 2)[0]
			if target == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(b.Out, target)); err != nil {
				b.problems = append(b.problems, fmt.Sprintf("%s links to %q which was not generated", filepath.Base(f), href))
			}
		}
	}
}

func hrefs(h string) []string {
	var out []string
	for {
		i := strings.Index(h, `href="`)
		if i < 0 {
			return out
		}
		h = h[i+6:]
		j := strings.IndexByte(h, '"')
		if j < 0 {
			return out
		}
		out = append(out, strings.ReplaceAll(h[:j], "&amp;", "&"))
		h = h[j:]
	}
}
