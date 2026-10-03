// SPDX-License-Identifier: Apache-2.0

package config

import (
	"unicode/utf8"

	"errors"
	toml "github.com/pelletier/go-toml/v2"
	"strings"
	"testing"
	"time"
)

const sample = `# repo-keeper configuration (hand written)
# keep this comment

[general]
root = "/home/u/code"   # where clones live
interval = "30m"

# the work account
[[account]]
name = "work"
provider = "gitlab"
base_url = "https://gitlab.example.com"
include = ["acme/**"]

# a manual repository
[[repo]]
path = "/home/u/dotfiles"

[cleanup]
mode = "dry-run"
`

func mustParse(t *testing.T, text string) Config {
	t.Helper()
	c, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, text)
	}
	return c
}

func TestSetAccount_AppendKeepsEverythingElse(t *testing.T) {
	out, err := SetAccount(sample, Account{Name: "personal", Provider: "github", Include: []string{"me/*"}, TokenFile: "/run/secrets/gh"})
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"# repo-keeper configuration (hand written)", "# keep this comment", "# where clones live", "# the work account", "# a manual repository", `base_url = "https://gitlab.example.com"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q:\n%s", keep, out)
		}
	}
	c := mustParse(t, out)
	if len(c.Accounts) != 2 || c.Accounts[1].Name != "personal" || c.Accounts[1].TokenFile != "/run/secrets/gh" || len(c.Repos) != 1 || c.Cleanup.Mode != "dry-run" {
		t.Fatalf("cfg=%+v", c)
	}
}

func TestSetAccount_ReplaceKeepsLeadingCommentAndPosition(t *testing.T) {
	out, err := SetAccount(sample, Account{Name: "work", Provider: "gitlab", BaseURL: "https://gl.new", Include: []string{"x/**"}, SkipForks: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# the work account") || strings.Contains(out, "gitlab.example.com") || !strings.Contains(out, "gl.new") {
		t.Fatalf("replace failed:\n%s", out)
	}
	if strings.Index(out, "[[account]]") > strings.Index(out, "[[repo]]") {
		t.Fatal("account moved after the repo block")
	}
	c := mustParse(t, out)
	if len(c.Accounts) != 1 || !c.Accounts[0].SkipForks || c.Accounts[0].BaseURL != "https://gl.new" {
		t.Fatalf("accounts=%+v", c.Accounts)
	}
}

func TestRemoveAccount_RemovesBlockAndItsComment_OnlyThat(t *testing.T) {
	out, err := RemoveAccount(sample, "work")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "# the work account") || strings.Contains(out, "gitlab") {
		t.Fatalf("block or its comment left behind:\n%s", out)
	}
	if !strings.Contains(out, "# a manual repository") || !strings.Contains(out, "[cleanup]") || !strings.Contains(out, "# keep this comment") {
		t.Fatalf("collateral damage:\n%s", out)
	}
	if c := mustParse(t, out); len(c.Accounts) != 0 || len(c.Repos) != 1 {
		t.Fatalf("cfg=%+v", c)
	}
	if _, err := RemoveAccount(sample, "ghost"); err == nil {
		t.Fatal("removing an unknown account must fail")
	}
}

func TestSetGeneralCleanupUI_ReplaceOrCreate(t *testing.T) {
	yes := true
	out, err := SetGeneral(sample, General{Root: "/data/clones", Interval: Duration(time.Hour), Concurrency: 6, PerHost: 3, AllBranches: &yes, QuietHours: "23:00-07:00"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "# where clones live") && strings.Contains(out, "/home/u/code") {
		t.Fatal("old general table survived")
	}
	c := mustParse(t, out)
	if c.General.Root != "/data/clones" || c.General.Concurrency != 6 || c.General.QuietHours != "23:00-07:00" || len(c.Accounts) != 1 {
		t.Fatalf("cfg=%+v", c.General)
	}
	out, err = SetCleanup(out, Cleanup{Mode: "auto", MinAge: Duration(14 * 24 * time.Hour), Protected: []string{"main", "release/*"}, AllowNeverPushed: true})
	if err != nil {
		t.Fatal(err)
	}
	c = mustParse(t, out)
	if c.Cleanup.Mode != "auto" || time.Duration(c.Cleanup.MinAge) != 14*24*time.Hour || len(c.Cleanup.Protected) != 2 || !c.Cleanup.AllowNeverPushed {
		t.Fatalf("cleanup=%+v", c.Cleanup)
	}
	if !strings.Contains(out, `min_age = "336h"`) {
		t.Fatalf("durations should read naturally:\n%s", out)
	}
	disabled := false
	out, err = SetUI("", UI{Enabled: &disabled, Port: 9000})
	if err != nil {
		t.Fatal(err)
	}
	if c := mustParse(t, out); c.UIEnabled() || c.UIPort() != 9000 {
		t.Fatalf("ui=%+v", c.UI)
	}
}

func TestSetRepo_AddReplaceRemove(t *testing.T) {
	out, err := SetRepo(sample, Repo{Path: "/home/u/new", CleanupMode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = SetRepo(out, Repo{Path: "/home/u/dotfiles", CleanupMode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	c := mustParse(t, out)
	if len(c.Repos) != 2 || c.Repos[0].CleanupMode != "off" || c.Repos[1].Path != "/home/u/new" {
		t.Fatalf("repos=%+v", c.Repos)
	}
	out, err = RemoveRepo(out, "/home/u/new")
	if err != nil {
		t.Fatal(err)
	}
	if c := mustParse(t, out); len(c.Repos) != 1 {
		t.Fatalf("repos=%+v", c.Repos)
	}
	if _, err := RemoveRepo(out, "/nope"); err == nil {
		t.Fatal("unknown repo must fail")
	}
}

func TestEdit_UnsafeLayoutsRefused_NotMangled(t *testing.T) {
	inline := "[general]\nroot = \"/r\"\naccount = [{ name = \"a\", provider = \"github\" }]\n"
	if _, err := Parse([]byte(strings.ReplaceAll(inline, "account =", "# x\naccount ="))); err == nil {
		// inline arrays of tables under [general] are unknown keys: confirm the premise before relying on it
		t.Log("premise: unknown key accepted?")
	}
	layout := "[general]\nroot = \"/r\"\n\n[[account]]\nname = \"a\"\nprovider = \"github\"\n"
	if _, err := SetAccount(layout, Account{Name: "a", Provider: "github", Include: []string{"x/*"}}); err != nil {
		t.Fatalf("normal layout must work: %v", err)
	}
	// top-level array written inline: valid TOML, but the editor cannot see the block, so it must refuse
	inlineTop := "account = [{ name = \"a\", provider = \"github\" }]\n[general]\nroot = \"/r\"\n"
	if _, err := Parse([]byte(inlineTop)); err != nil {
		t.Fatalf("test premise: inline layout should be valid TOML: %v", err)
	}
	if _, err := SetAccount(inlineTop, Account{Name: "a", Provider: "github", Include: []string{"x/*"}}); !errors.Is(err, ErrUnsupportedLayout) && err == nil {
		t.Fatal("a layout the editor cannot see must not be silently duplicated")
	}
}

func TestEdit_ResultMustBeValid_AndInputUntouchedOnError(t *testing.T) {
	// an account needs general.root; the edit that would leave the file invalid is rejected with the real reason
	_, err := SetAccount("", Account{Name: "x", Provider: "github"})
	if err == nil || !strings.Contains(err.Error(), "general.root is required") {
		t.Fatalf("err=%v", err)
	}
	if _, err := SetCleanup(sample, Cleanup{Mode: "yolo"}); err == nil {
		t.Fatal("invalid mode must be rejected")
	}
	if _, err := SetAccount(sample, Account{Name: "Bad Name", Provider: "github"}); err == nil {
		t.Fatal("invalid account name must be rejected")
	}
}

func TestSplit_MultilineStringsAreNotHeaders(t *testing.T) {
	text := "[general]\nroot = \"/r\"\n\n[[account]]\nname = \"a\"\nprovider = \"github\"\ninclude = [\"x/*\"]\n"
	out, err := SetAccount(text, Account{Name: "a", Provider: "github", Include: []string{"y/*"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "[[account]]") != 1 {
		t.Fatalf("duplicated block:\n%s", out)
	}
	blocks := split("[general]\nnote = '''\n[[account]]\nfake\n'''\nroot = \"/r\"\n")
	if len(blocks) != 2 || blocks[1].name != "general" {
		t.Fatalf("a header-looking line inside a multi-line string was treated as a table: %+v", blocks)
	}
}

func TestDurations(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Minute: "30m", time.Hour: "1h", 168 * time.Hour: "168h", 90 * time.Second: "1m30s", 0: "0s"} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "7d": 7 * 24 * time.Hour, "1.5d": 36 * time.Hour, " 12h ": 12 * time.Hour} {
		if got, err := ParseFlexibleDuration(in); err != nil || got != want {
			t.Errorf("ParseFlexibleDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "soon", "-3d", "d", "99999999d"} {
		if _, err := ParseFlexibleDuration(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func FuzzEdit_NeverPanics_AndSuccessAlwaysParses(f *testing.F) {
	for _, s := range []string{sample, "", "[general", "[[account]]\nname=", "x = '''\n[[account]]", "[[repo]]\npath=\"/a\"\n[[repo]]\npath=\"/a\""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		for _, op := range []func(string) (string, error){
			func(s string) (string, error) { return SetAccount(s, Account{Name: "fz", Provider: "github"}) },
			func(s string) (string, error) { return RemoveAccount(s, "fz") },
			func(s string) (string, error) { return SetRepo(s, Repo{Path: "/fz"}) },
			func(s string) (string, error) { return RemoveRepo(s, "/fz") },
			func(s string) (string, error) { return SetCleanup(s, Cleanup{Mode: "off"}) },
		} {
			out, err := op(text)
			if err == nil {
				if _, perr := Parse([]byte(out)); perr != nil {
					t.Fatalf("edit reported success but produced an invalid file: %v\n%s", perr, out)
				}
			}
		}
	})
}

func FuzzQuote_RoundTripsThroughTheRealParser(f *testing.F) {
	for _, s := range []string{"plain", `quo"te`, `back\slash`, "new\nline", "tab\t", "bell\a", "\x7f", "日本語", " ", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			t.Skip()
		}
		var got struct {
			X string `toml:"x"`
		}
		if err := toml.Unmarshal([]byte("x = "+quote(s)), &got); err != nil || got.X != s {
			t.Fatalf("quote(%q) did not survive the parser: got %q err %v", s, got.X, err)
		}
	})
}

func TestAccount_OAuthFields_ValidatedAndPersisted(t *testing.T) {
	root := "[general]\nroot = \"/r\"\n"
	out, err := SetAccount(root, Account{Name: "gh", Provider: "github", OAuthClientID: "Iv1.0123456789abcdef", OAuthWebURL: "https://ghe.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if c := mustParse(t, out); c.Accounts[0].OAuthClientID != "Iv1.0123456789abcdef" || c.Accounts[0].OAuthWebURL != "https://ghe.example.com" {
		t.Fatalf("not persisted: %+v", c.Accounts[0])
	}
	for _, bad := range []Account{
		{Name: "gl", Provider: "gitlab", OAuthClientID: "Iv1.0123456789abcdef"}, // device flow is GitHub only
		{Name: "gh", Provider: "github", OAuthClientID: "x"},
		{Name: "gh", Provider: "github", OAuthClientID: "has space"},
		{Name: "gh", Provider: "github", OAuthWebURL: "http://example.com"},
	} {
		if _, err := SetAccount(root, bad); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

func TestSetCleanup_ZeroMinAgeIsWrittenExplicitly(t *testing.T) {
	out, err := SetCleanup(sample, Cleanup{Mode: "auto", MinAge: 0, Protected: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	if c := mustParse(t, out); time.Duration(c.Cleanup.MinAge) != 0 || !strings.Contains(out, `min_age = "0s"`) {
		t.Fatalf("a zero minimum age must survive (the default is 7d):\n%s", out)
	}
}
