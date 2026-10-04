// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// run executes the built binary in an empty, private environment and returns its combined output.
func (b *build) run(args ...string) (string, error) {
	home, err := os.MkdirTemp("", "site-home-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(home) }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// #nosec G204 -- the binary this build was asked to document, with fixed arguments
	cmd := exec.CommandContext(ctx, b.Bin, args...) //nolint:gosec,forbidigo // sanctioned: this tool exists to run that binary
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "XDG_RUNTIME_DIR=" + home}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run() // flag usage exits non-zero; the text is what matters
	return strings.TrimRight(out.String(), "\n"), ctx.Err()
}

type command struct{ Name, Description string }

// commands parses the "Commands:" block of the binary's usage text; the name column is separated by two or more spaces.
func parseCommands(usage string) []command {
	var out []command
	in := false
	sep := regexp.MustCompile(`\s{2,}`)
	sc := bufio.NewScanner(strings.NewReader(usage))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Commands:"):
			in = true
		case in && strings.TrimSpace(line) == "":
			return out
		case in:
			if parts := sep.Split(strings.TrimSpace(line), 2); len(parts) == 2 {
				out = append(out, command{parts[0], parts[1]})
			}
		}
	}
	return out
}

// expand turns "start | stop | restart" and "accounts add|list" into one entry per invocable command.
func expand(name string) []string {
	if strings.Contains(name, " | ") {
		return strings.Split(name, " | ")
	}
	if i := strings.Index(name, "|"); i > 0 {
		head := name[:strings.LastIndex(name[:i], " ")+1]
		var out []string
		for _, w := range strings.Split(name[len(head):], "|") {
			out = append(out, head+w)
		}
		return out
	}
	return []string{name}
}

func words(cmd string) (invoke []string) {
	for _, w := range strings.Fields(cmd) {
		if !strings.HasPrefix(w, "<") {
			invoke = append(invoke, w)
		}
	}
	return invoke
}

func genCLI(b *build) (string, error) {
	usage, err := b.run()
	if err != nil {
		return "", err
	}
	cmds := parseCommands(usage)
	if len(cmds) == 0 {
		return "", fmt.Errorf("no commands found in the usage text:\n%s", usage)
	}
	var h strings.Builder
	h.WriteString("<p>Generated from the <code>" + html.EscapeString(b.Version) + "</code> binary. Run <code>repo-keeper &lt;command&gt; -h</code> for the same text.</p>\n")
	h.WriteString("<h2 id=\"commands\">Commands</h2>\n<div class=\"table-wrap\" tabindex=\"0\"><table><thead><tr><th>Command</th><th>What it does</th></tr></thead><tbody>\n")
	for _, c := range cmds {
		fmt.Fprintf(&h, "<tr><td><code>%s</code></td><td>%s</td></tr>\n", html.EscapeString(c.Name), html.EscapeString(c.Description))
	}
	h.WriteString("</tbody></table></div>\n")
	for _, c := range cmds {
		for _, one := range expand(c.Name) {
			text, err := b.run(append(words(one), "-h")...)
			if err != nil {
				return "", err
			}
			id := strings.NewReplacer(" ", "-", "<", "", ">", "").Replace(one)
			fmt.Fprintf(&h, "<h2 id=\"%s\"><code>repo-keeper %s</code></h2>\n<p>%s</p>\n", html.EscapeString(id), html.EscapeString(one), html.EscapeString(c.Description))
			if strings.HasPrefix(text, "Usage of ") {
				fmt.Fprintf(&h, "<pre><code>%s</code></pre>\n", html.EscapeString(text))
			} else {
				h.WriteString("<p class=\"muted\">This command takes no flags.</p>\n")
			}
		}
	}
	return h.String(), nil
}

func genConfig(b *build) (string, error) {
	dir, err := os.MkdirTemp("", "site-config-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "config.toml")
	if out, err := b.run("init", "--config", path); err != nil {
		return "", fmt.Errorf("init: %w\n%s", err, out)
	}
	// #nosec G304 -- temp file we just had the binary write
	data, err := os.ReadFile(path) //nolint:gosec // see #nosec above
	if err != nil {
		return "", fmt.Errorf("the binary did not write a starter configuration: %w", err)
	}
	return `<p>The configuration is one TOML file (default <code>~/.config/repo-keeper/config.toml</code>). Unknown keys are errors, so a typo never silently weakens a policy; check a file with <code>repo-keeper config validate</code>. Accounts hold the platform settings (see <a href="providers.html">Providers</a> for the per-platform fields); credentials never go in this file.</p>
<h2 id="starter">Starter configuration</h2>
<p>This is exactly what <code>repo-keeper init</code> writes in version <code>` + html.EscapeString(b.Version) + `</code>.</p>
<pre><code>` + html.EscapeString(string(data)) + `</code></pre>
`, nil
}

type release struct {
	Tag        string    `json:"tag_name"`
	Published  time.Time `json:"published_at"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	URL        string    `json:"html_url"`
	Assets     []struct {
		Name  string `json:"name"`
		URL   string `json:"browser_download_url"`
		Count int    `json:"download_count"`
	} `json:"assets"`
}

func (b *build) loadReleases() ([]release, error) {
	if b.Releases == "" {
		return nil, nil
	}
	data, err := os.ReadFile(b.Releases)
	if err != nil {
		return nil, err
	}
	var rs []release
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("releases JSON: %w", err)
	}
	var out []release
	for _, r := range rs {
		if !r.Draft {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Published.After(out[j].Published) })
	return out, nil
}

// genReleases appends the downloads of published releases to the changelog page.
func genReleases(b *build) (string, error) {
	rs, err := b.loadReleases()
	if err != nil || len(rs) == 0 {
		return "", err
	}
	var h strings.Builder
	h.WriteString("<h2 id=\"downloads\">Downloads</h2>\n<p>Published releases, newest first. Always verify what you download (checksums, signature, provenance): see <a href=\"install.html\">Install</a>.</p>\n")
	h.WriteString("<div class=\"table-wrap\" tabindex=\"0\"><table><thead><tr><th>Release</th><th>Date</th><th>Files</th></tr></thead><tbody>\n")
	for i, r := range rs {
		if i == 10 {
			break
		}
		kind := ""
		if r.Prerelease {
			kind = " <span class=\"tag\">pre-release</span>"
		}
		var files []string
		for _, a := range r.Assets {
			if strings.HasSuffix(a.Name, ".sig") || strings.HasSuffix(a.Name, ".pem") || strings.HasSuffix(a.Name, ".spdx.json") {
				continue
			}
			files = append(files, fmt.Sprintf("<a href=\"%s\">%s</a>", html.EscapeString(a.URL), html.EscapeString(a.Name)))
		}
		fmt.Fprintf(&h, "<tr><td><a href=\"%s\">%s</a>%s</td><td>%s</td><td>%s</td></tr>\n", html.EscapeString(r.URL), html.EscapeString(r.Tag), kind, r.Published.Format("2006-01-02"), strings.Join(files, "<br>"))
	}
	h.WriteString("</tbody></table></div>\n")
	return h.String(), nil
}
