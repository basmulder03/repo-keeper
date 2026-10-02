// SPDX-License-Identifier: Apache-2.0

package askpass

import (
	"bytes"
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
