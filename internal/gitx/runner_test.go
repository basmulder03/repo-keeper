// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"os"
	"strings"
	"testing"
)

func TestParseVersion_Outputs(t *testing.T) {
	tests := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"git version 2.43.0", Version{2, 43, 0}, true},
		{"git version 2.39.3 (Apple Git-146)", Version{2, 39, 3}, true},
		{"git version 2.45.1.windows.1", Version{2, 45, 1}, true},
		{"git version 2.50", Version{2, 50, 0}, true},
		{"nope", Version{}, false},
	}
	for _, tc := range tests {
		got, err := ParseVersion(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("%q => %v, %v", tc.in, got, err)
		}
	}
}

func TestVersion_AtLeast_Ordering(t *testing.T) {
	if !(Version{2, 34, 0}).AtLeast(MinVersion) || (Version{2, 33, 9}).AtLeast(MinVersion) || !(Version{3, 0, 0}).AtLeast(MinVersion) {
		t.Fatal("ordering wrong")
	}
}

func TestBuildEnv_DropsSecretsAndGitVars_KeepsHardening(t *testing.T) {
	base := []string{
		"PATH=/bin", "HOME=/h", "GITHUB_TOKEN=CANARY", "GIT_DIR=/evil", "GIT_SSH_COMMAND=evil",
		"GIT_ASKPASS=evil", "AWS_SECRET_ACCESS_KEY=CANARY",
	}
	env := strings.Join(buildEnv(base, []string{"HOME=/override"}), "\n")
	for _, bad := range []string{"CANARY", "GIT_DIR", "GIT_SSH_COMMAND", "GIT_ASKPASS"} {
		if strings.Contains(env, bad) {
			t.Errorf("env leaks %s:\n%s", bad, env)
		}
	}
	for _, want := range []string{"PATH=/bin", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "core.hooksPath", "core.fsmonitor"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %s", want)
		}
	}
	if !strings.HasSuffix(env, "HOME=/override") {
		t.Error("overrides must come last")
	}
}

func TestError_Message_ScrubsTokens(t *testing.T) {
	r := &Runner{Bin: "git", Env: []string{"HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}}
	_, err := r.Run(t.Context(), t.TempDir(), "ls-remote", "https://user:s3cretpw@127.0.0.1:1/x.git")
	if err == nil {
		t.Skip("unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "s3cretpw") {
		t.Fatalf("password leaked: %v", err)
	}
}
