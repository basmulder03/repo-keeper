// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_Commands_ExitCodesAndOutput(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		code    int
		wantOut string
		wantErr string
	}{
		{"version", []string{"version"}, 0, "repo-keeper dev", ""},
		{"help", []string{"help"}, 0, "Usage:", ""},
		{"no args", nil, 2, "", "Usage:"},
		{"unknown", []string{"nope"}, 2, "", "unknown command"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := run(tc.args, &out, &errb); got != tc.code {
				t.Fatalf("code = %d, want %d", got, tc.code)
			}
			if !strings.Contains(out.String(), tc.wantOut) || !strings.Contains(errb.String(), tc.wantErr) {
				t.Fatalf("stdout=%q stderr=%q", out.String(), errb.String())
			}
		})
	}
}
