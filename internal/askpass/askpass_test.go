// SPDX-License-Identifier: Apache-2.0

package askpass

import (
	"bytes"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRun_AnswersOnlyForConfiguredHost(t *testing.T) {
	e := env(map[string]string{EnvHost: "github.com", EnvUser: "x-access-token", EnvSecret: "tok123"})
	tests := []struct {
		prompt string
		code   int
		want   string
	}{
		{"Username for 'https://github.com': ", 0, "x-access-token\n"},
		{"Password for 'https://x-access-token@github.com': ", 0, "tok123\n"},
		{"Password for 'https://GitHub.com': ", 0, "tok123\n"},
		{"Password for 'https://evil.example': ", 1, ""},
		{"Password for 'https://github.com.evil.example': ", 1, ""},
		{"Password for 'https://evil.example@github.com@evil.example': ", 1, ""},
		{"Enter passphrase for key '/home/u/.ssh/id': ", 1, ""},
		{"Password for no-quotes: ", 1, ""},
		{"", 1, ""},
	}
	for _, tc := range tests {
		var out bytes.Buffer
		if code := Run([]string{tc.prompt}, e, &out); code != tc.code || out.String() != tc.want {
			t.Errorf("%q => code=%d out=%q", tc.prompt, code, out.String())
		}
	}
	if Run(nil, e, &bytes.Buffer{}) != 1 {
		t.Error("no prompt must fail")
	}
}

func TestRun_PortIsPartOfHost(t *testing.T) {
	e := env(map[string]string{EnvHost: "127.0.0.1:8080", EnvSecret: "s"})
	var out bytes.Buffer
	if Run([]string{"Password for 'http://127.0.0.1:8080': "}, e, &out) != 0 || Run([]string{"Password for 'http://127.0.0.1:9999': "}, e, &out) != 1 {
		t.Fatal("port must match")
	}
}

func TestRun_NeverDiscloses_InCleartextToRemoteHosts(t *testing.T) {
	for _, tc := range []struct {
		host, prompt string
		want         int
	}{
		{"example.com", "Password for 'http://example.com': ", 1},
		{"example.com:8080", "Password for 'http://user@example.com:8080': ", 1},
		{"203.0.113.9", "Password for 'http://203.0.113.9': ", 1},
		{"git.example.com", "Password for 'git://git.example.com': ", 1},
		{"example.com", "Password for 'https://example.com': ", 0},
		{"127.0.0.1:9", "Password for 'http://127.0.0.1:9': ", 0},
		{"[::1]:9", "Password for 'http://[::1]:9': ", 0},
	} {
		var out bytes.Buffer
		e := env(map[string]string{EnvHost: tc.host, EnvSecret: "tok"})
		if got := Run([]string{tc.prompt}, e, &out); got != tc.want || (tc.want == 1 && out.Len() != 0) {
			t.Errorf("%q => %d %q", tc.prompt, got, out.String())
		}
	}
}

func FuzzRun_NeverPanicsAndNeverLeaksToOtherHosts(f *testing.F) {
	for _, s := range []string{"Password for 'https://github.com': ", "Username for 'http://127.0.0.1:1'", "'", "''", "Password for '://': ", "Password for 'https://a@b@github.com'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, prompt string) {
		var out bytes.Buffer
		e := env(map[string]string{EnvHost: "github.com", EnvUser: "u", EnvSecret: "SECRET-VALUE"})
		code := Run([]string{prompt}, e, &out)
		if code != 0 && out.Len() != 0 {
			t.Fatalf("output on failure: %q", out.String())
		}
		if code == 0 {
			scheme, host := promptTarget(prompt)
			if !strings.EqualFold(host, "github.com") || scheme != "https" {
				t.Fatalf("answered a prompt for %q://%q: %q", scheme, host, prompt)
			}
		}
	})
}
