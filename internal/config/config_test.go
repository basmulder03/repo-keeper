// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse_Starter_IsValid(t *testing.T) {
	c, err := Parse([]byte(Starter))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(c.General.Interval) != 30*time.Minute || c.Cleanup.Mode != "dry-run" {
		t.Fatalf("%+v", c)
	}
}

func TestParse_EmptyFile_UsesDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil || c.General.Concurrency != 4 || len(c.Cleanup.Protected) == 0 {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

func TestParse_Rejections(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"interval floor":   {"[general]\ninterval = \"1m\"", "below the 5m0s minimum"},
		"bad mode":         {"[cleanup]\nmode = \"yolo\"", "must be off, dry-run or auto"},
		"unknown key":      {"[cleanup]\nmodee = \"auto\"", "modee"},
		"relative repo":    {"[[repo]]\npath = \"rel/path\"", "absolute path"},
		"duplicate repo":   {"[[repo]]\npath = \"/a\"\n[[repo]]\npath = \"/a/\"", "listed twice"},
		"repo interval":    {"[[repo]]\npath = \"/a\"\ninterval = \"1m\"", "below the"},
		"repo bad cleanup": {"[[repo]]\npath = \"/a\"\ncleanup = \"x\"", "must be off"},
		"concurrency":      {"[general]\nconcurrency = 0", "concurrency"},
		"quiet hours":      {"[general]\nquiet_hours = \"nope\"", "quiet_hours"},
		"bad toml":         {"[general", "config:"},
		"bad duration":     {"[general]\ninterval = \"soon\"", "config:"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestParse_ReportsAllProblemsAtOnce(t *testing.T) {
	_, err := Parse([]byte("[general]\ninterval = \"1m\"\nconcurrency = 99\n[cleanup]\nmode = \"x\""))
	if err == nil || strings.Count(err.Error(), "\n") < 2 {
		t.Fatalf("err=%v", err)
	}
}

func TestResolve_OverridesAndDefaults(t *testing.T) {
	c, _ := Parse([]byte("[general]\ninterval = \"1h\"\n[cleanup]\nmode = \"dry-run\"\n[[repo]]\npath = \"/a\"\n[[repo]]\npath = \"/b\"\ninterval = \"10m\"\nall_branches = false\ncleanup = \"auto\"\nremote = \"up\""))
	a, b := c.Resolve(c.Repos[0]), c.Resolve(c.Repos[1])
	if a.Interval != time.Hour || !a.AllBranches || a.Policy.Mode != "dry-run" || a.Remote != "origin" {
		t.Fatalf("a=%+v", a)
	}
	if b.Interval != 10*time.Minute || b.AllBranches || b.Policy.Mode != "auto" || b.Remote != "up" {
		t.Fatalf("b=%+v", b)
	}
}

func TestLoad_FileAndMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	if _, err := Load(p); err == nil {
		t.Fatal("missing file must error")
	}
	_ = os.WriteFile(p, []byte(Starter), 0o600)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	wrap, err := ParseQuietHours("23:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	day, _ := ParseQuietHours("09:00-17:30")
	for _, tc := range []struct {
		q    QuietHours
		t    time.Time
		want bool
	}{
		{wrap, at(23, 0), true}, {wrap, at(3, 0), true}, {wrap, at(7, 0), false}, {wrap, at(12, 0), false},
		{day, at(9, 0), true}, {day, at(17, 29), true}, {day, at(17, 30), false}, {day, at(8, 59), false},
		{QuietHours{}, at(3, 0), false},
	} {
		if got := tc.q.Contains(tc.t); got != tc.want {
			t.Errorf("%+v at %v = %v", tc.q, tc.t, got)
		}
	}
	for _, bad := range []string{"", "9-17", "25:00-01:00", "09:00"} {
		if _, err := ParseQuietHours(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func FuzzParse_NeverPanics(f *testing.F) {
	f.Add([]byte(Starter))
	f.Add([]byte("[[repo]]\npath=\"/x\""))
	f.Add([]byte("\x00\xff[["))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Parse(b) })
}

func TestParse_Accounts_ValidAndResolved(t *testing.T) {
	c, err := Parse([]byte(`
[general]
root = "/home/u/code"
[[account]]
name = "work-gh"
provider = "github"
base_url = "https://ghe.example.com/api/v3"
token_file = "/run/secrets/gh"
include = ["acme/*"]
exclude = ["*/archive-*"]
skip_archived = false
clone_protocol = "ssh"
discovery_interval = "12h"
interval = "1h"
cleanup = "auto"
[[account]]
name = "p"
provider = "github"
`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := c.ResolveAccount(c.Accounts[0]), c.ResolveAccount(c.Accounts[1])
	if a.SkipArchivedRepos || !a.UseSSH || a.Discovery != 12*time.Hour || a.SyncInterval != time.Hour || a.Policy.Mode != "auto" {
		t.Fatalf("a=%+v", a)
	}
	if !b.SkipArchivedRepos || b.UseSSH || b.Discovery != 6*time.Hour || b.SyncInterval != 30*time.Minute || b.Policy.Mode != "dry-run" {
		t.Fatalf("b=%+v", b)
	}
}

func TestParse_Accounts_Rejections(t *testing.T) {
	root := "[general]\nroot = \"/r\"\n"
	tests := map[string]struct{ in, want string }{
		"no root":         {"[[account]]\nname=\"a\"\nprovider=\"github\"", "general.root is required"},
		"relative root":   {"[general]\nroot=\"r\"", "general.root must be an absolute"},
		"bad name":        {root + "[[account]]\nname=\"Bad Name\"\nprovider=\"github\"", "must match"},
		"duplicate":       {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\n[[account]]\nname=\"a\"\nprovider=\"github\"", "used twice"},
		"provider":        {root + "[[account]]\nname=\"a\"\nprovider=\"svn\"", "not supported"},
		"both token srcs": {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\ntoken_env=\"X\"\ntoken_file=\"/f\"", "only one of"},
		"relative file":   {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\ntoken_file=\"f\"", "absolute"},
		"http base":       {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\nbase_url=\"http://example.com\"", "https"},
		"protocol":        {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\nclone_protocol=\"ftp\"", "https or ssh"},
		"discovery floor": {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\ndiscovery_interval=\"5m\"", "discovery_interval"},
		"interval floor":  {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\ninterval=\"1m\"", "below"},
		"cleanup":         {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\ncleanup=\"x\"", "must be off"},
		"glob":            {root + "[[account]]\nname=\"a\"\nprovider=\"github\"\ninclude=[\"[\"]", "invalid glob"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestParse_LoopbackHTTPBaseURL_Allowed(t *testing.T) {
	if _, err := Parse([]byte("[general]\nroot=\"/r\"\n[[account]]\nname=\"t\"\nprovider=\"github\"\nbase_url=\"http://127.0.0.1:8080\"")); err != nil {
		t.Fatal(err)
	}
}

func TestParse_Accounts_GitLabAccepted_UnknownProviderListsChoices(t *testing.T) {
	root := "[general]\nroot = \"/r\"\n"
	if _, err := Parse([]byte(root + "[[account]]\nname=\"gl\"\nprovider=\"gitlab\"\nbase_url=\"https://gitlab.example.com\"\ninclude=[\"acme/**\"]")); err != nil {
		t.Fatalf("gitlab rejected: %v", err)
	}
	_, err := Parse([]byte(root + "[[account]]\nname=\"x\"\nprovider=\"svn\""))
	if err == nil || !strings.Contains(err.Error(), "github, gitlab") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoad_OversizedFile_Refused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte("# "+strings.Repeat("x", maxConfigBytes)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err=%v", err)
	}
}

func FuzzParseQuietHours_NeverPanics(f *testing.F) {
	for _, s := range []string{"23:00-07:00", "", "-", "99:99-00:00", "1:1-2:2", "\x00-\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := ParseQuietHours(s)
		if err == nil {
			_ = q.Contains(time.Now())
		}
	})
}

func TestParse_SecretsBackend(t *testing.T) {
	if _, err := Parse([]byte("[general]\nsecrets = \"file\"\nsecrets_file = \"/var/lib/rk/secrets.enc\"")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"[general]\nsecrets = \"vault\"", "[general]\nsecrets_file = \"rel/path\""} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestParse_Accounts_GiteaForgejo_NeedBaseURL(t *testing.T) {
	root := "[general]\nroot = \"/r\"\n"
	for _, p := range []string{"gitea", "forgejo"} {
		acct := "[[account]]\nname=\"a\"\nprovider=\"" + p + "\"\n"
		if _, err := Parse([]byte(root + acct)); err == nil || !strings.Contains(err.Error(), "base_url is required") {
			t.Errorf("%s without base_url: %v", p, err)
		}
		if _, err := Parse([]byte(root + acct + "base_url=\"https://codeberg.org\"\n")); err != nil {
			t.Errorf("%s with base_url rejected: %v", p, err)
		}
	}
}

func TestParse_Accounts_BitbucketAccepted_BaseURLOptional(t *testing.T) {
	root := "[general]\nroot = \"/r\"\n"
	if _, err := Parse([]byte(root + "[[account]]\nname=\"bb\"\nprovider=\"bitbucket\"\ninclude=[\"acme/*\"]\n")); err != nil {
		t.Fatalf("bitbucket rejected: %v", err)
	}
}
