// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// The functions below change a config file's text surgically: only the table being edited is replaced, removed
// or appended, so comments, ordering and unrelated tables survive. Every result is re-parsed with the real
// validator and checked to contain exactly the intended change; a layout the editor cannot handle safely
// (inline tables, dotted keys...) is refused with ErrUnsupportedLayout instead of being mangled.

// ErrUnsupportedLayout means the file is valid but laid out in a way the form editor cannot safely modify.
var ErrUnsupportedLayout = errors.New("this config file uses a layout the form editor cannot modify safely (for example inline tables or dotted keys); use the raw editor instead")

var headerRe = regexp.MustCompile(`^\s*(\[\[?)\s*([A-Za-z0-9_.\-]+)\s*\]\]?\s*(#.*)?$`)

type block struct {
	name    string   // table name, "" for the preamble
	array   bool     // [[name]]
	leading []string // comment/blank lines directly above the header (belong to this block)
	body    []string // header line and everything up to the next block's leading lines
}

func (b block) text() string {
	return strings.Join(append(append([]string{}, b.leading...), b.body...), "\n")
}

// split cuts text into a preamble and table blocks. Lines inside multi-line strings are never mistaken for headers.
func split(text string) []block {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	blocks := []block{{}}
	inMulti := ""
	for _, ln := range lines {
		if inMulti != "" {
			blocks[len(blocks)-1].body = append(blocks[len(blocks)-1].body, ln)
			if strings.Contains(ln, inMulti) {
				inMulti = ""
			}
			continue
		}
		if m := headerRe.FindStringSubmatch(ln); m != nil {
			nb := block{name: m[2], array: m[1] == "[[", body: []string{ln}}
			// steal trailing comment/blank lines from the previous block: they describe this table
			prev := &blocks[len(blocks)-1]
			i := len(prev.body)
			for i > 0 {
				t := strings.TrimSpace(prev.body[i-1])
				if t == "" || strings.HasPrefix(t, "#") {
					i--
					continue
				}
				break
			}
			nb.leading = append(nb.leading, prev.body[i:]...)
			prev.body = prev.body[:i]
			blocks = append(blocks, nb)
			continue
		}
		cur := &blocks[len(blocks)-1]
		cur.body = append(cur.body, ln)
		for _, q := range []string{`"""`, `'''`} {
			if strings.Count(ln, q)%2 == 1 {
				inMulti = q
			}
		}
	}
	return blocks
}

func join(blocks []block) string {
	var parts []string
	for _, b := range blocks {
		if t := b.text(); t != "" || len(parts) > 0 {
			parts = append(parts, t)
		}
	}
	return strings.TrimRight(strings.Join(parts, "\n"), "\n") + "\n"
}

// render writes one table (header "[general]") or array-of-tables element (header "[[account]]") from a config struct:
// keys in field order, zero values omitted, plain "double quoted" strings, durations as people write them.
func render(header string, v any) string {
	var b strings.Builder
	b.WriteString(header + "\n")
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		key := strings.Split(rt.Field(i).Tag.Get("toml"), ",")[0]
		f := rv.Field(i)
		if key == "" || key == "-" {
			continue
		}
		if val, ok := tomlValue(f); ok {
			b.WriteString(key + " = " + val + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func tomlValue(f reflect.Value) (string, bool) {
	switch v := f.Interface().(type) {
	case string:
		return quote(v), v != ""
	case bool:
		return "true", v
	case int:
		return fmt.Sprint(v), v != 0
	case *bool:
		if v == nil {
			return "", false
		}
		return fmt.Sprint(*v), true
	case Duration:
		return quote(FormatDuration(time.Duration(v))), v != 0
	case []string:
		if len(v) == 0 {
			return "", false
		}
		q := make([]string, len(v))
		for i, s := range v {
			q[i] = quote(s)
		}
		return "[" + strings.Join(q, ", ") + "]", true
	}
	return "", false
}

// quote renders a TOML basic string (Go's %q is not TOML-safe: it emits \a, \v and \x escapes).
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func blockName(b block, key string) string {
	var probe struct {
		Account []map[string]any `toml:"account"`
		Repo    []map[string]any `toml:"repo"`
	}
	if toml.Unmarshal([]byte(strings.Join(b.body, "\n")), &probe) != nil {
		return ""
	}
	list := probe.Account
	if b.name == "repo" {
		list = probe.Repo
	}
	if len(list) == 1 {
		if s, ok := list[0][key].(string); ok {
			return s
		}
	}
	return ""
}

// replaceOrAppend swaps the block matching pick, or appends rendered when none matches.
func replaceOrAppend(text, table string, array bool, pick func(block) bool, rendered string) string {
	blocks := split(text)
	for i, b := range blocks {
		if b.name == table && b.array == array && pick(b) {
			blocks[i].body = strings.Split(rendered, "\n") // keep the user's comments above the header
			return join(blocks)
		}
	}
	nb := block{name: table, array: array, body: strings.Split(rendered, "\n")}
	if len(blocks) > 1 || strings.TrimSpace(blocks[0].text()) != "" {
		nb.leading = []string{""}
	}
	return join(append(blocks, nb))
}

func remove(text, table string, array bool, pick func(block) bool) (string, bool) {
	blocks := split(text)
	for i, b := range blocks {
		if b.name == table && b.array == array && pick(b) {
			return join(append(blocks[:i:i], blocks[i+1:]...)), true
		}
	}
	return text, false
}

func verify(text string, check func(Config) error) (string, error) {
	cfg, err := Parse([]byte(text))
	if err != nil {
		return "", err
	}
	if err := check(cfg); err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnsupportedLayout, err)
	}
	return text, nil
}

// SetGeneral replaces (or creates) the [general] table.
func SetGeneral(text string, g General) (string, error) {
	r := render("[general]", g)
	out := replaceOrAppend(text, "general", false, func(block) bool { return true }, r)
	return verify(out, func(c Config) error {
		if !reflect.DeepEqual(normGeneral(c.General), normGeneral(g)) {
			return errors.New("general table did not round-trip")
		}
		return nil
	})
}

// SetCleanup replaces (or creates) the [cleanup] table.
func SetCleanup(text string, c Cleanup) (string, error) {
	r := render("[cleanup]", c)
	out := replaceOrAppend(text, "cleanup", false, func(block) bool { return true }, r)
	return verify(out, func(cfg Config) error {
		if !reflect.DeepEqual(cfg.Cleanup, c) && (len(c.Protected) != 0 || len(cfg.Cleanup.Protected) != 0) {
			return errors.New("cleanup table did not round-trip")
		}
		return nil
	})
}

// SetUI replaces (or creates) the [ui] table.
func SetUI(text string, u UI) (string, error) {
	r := render("[ui]", u)
	out := replaceOrAppend(text, "ui", false, func(block) bool { return true }, r)
	return verify(out, func(c Config) error {
		if !reflect.DeepEqual(c.UI, u) {
			return errors.New("ui table did not round-trip")
		}
		return nil
	})
}

// SetAccount replaces the [[account]] block with the same name, or appends a new one.
func SetAccount(text string, a Account) (string, error) {
	r := render("[[account]]", a)
	out := replaceOrAppend(text, "account", true, func(b block) bool { return blockName(b, "name") == a.Name }, r)
	return verify(out, func(c Config) error {
		for _, got := range c.Accounts {
			if got.Name == a.Name {
				if !reflect.DeepEqual(normAccount(got), normAccount(a)) {
					return errors.New("account did not round-trip")
				}
				return nil
			}
		}
		return errors.New("account missing after edit")
	})
}

// RemoveAccount deletes the [[account]] block with that name.
func RemoveAccount(text, name string) (string, error) {
	out, ok := remove(text, "account", true, func(b block) bool { return blockName(b, "name") == name })
	if !ok {
		return "", fmt.Errorf("account %q not found as an [[account]] table", name)
	}
	return verify(out, func(c Config) error {
		for _, a := range c.Accounts {
			if a.Name == name {
				return errors.New("account still present after removal")
			}
		}
		return nil
	})
}

// SetRepo replaces the [[repo]] block with the same path, or appends a new one.
func SetRepo(text string, r Repo) (string, error) {
	rendered := render("[[repo]]", r)
	out := replaceOrAppend(text, "repo", true, func(b block) bool { return blockName(b, "path") == r.Path }, rendered)
	return verify(out, func(c Config) error {
		for _, got := range c.Repos {
			if got.Path == r.Path {
				return nil
			}
		}
		return errors.New("repo missing after edit")
	})
}

// RemoveRepo deletes the [[repo]] block with that path.
func RemoveRepo(text, path string) (string, error) {
	out, ok := remove(text, "repo", true, func(b block) bool { return blockName(b, "path") == path })
	if !ok {
		return "", fmt.Errorf("repository %q not found as a [[repo]] table", path)
	}
	return verify(out, func(c Config) error {
		for _, r := range c.Repos {
			if r.Path == path {
				return errors.New("repo still present after removal")
			}
		}
		return nil
	})
}

func normGeneral(g General) General {
	if g.AllBranches != nil && *g.AllBranches {
		t := true
		g.AllBranches = &t
	}
	return g
}

func normAccount(a Account) Account {
	if len(a.Include) == 0 {
		a.Include = nil
	}
	if len(a.Exclude) == 0 {
		a.Exclude = nil
	}
	return a
}

// FormatDuration renders a duration the way people write it in config files: 30m, 1h, 168h.
func FormatDuration(d time.Duration) string {
	s := d.String() // e.g. 30m0s, 1h0m0s, 1m30s
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// ParseFlexibleDuration accepts Go durations plus a "d" (days) suffix: "30m", "12h", "7d".
func ParseFlexibleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days float64
		if _, err := fmt.Sscanf(n, "%g", &days); err != nil || days < 0 || days > 36500 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (examples: 30m, 12h, 7d)", s)
	}
	return d, nil
}
