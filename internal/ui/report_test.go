// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/url"
	"strings"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/store"
)

func TestReportURL_FailedClone_PrefillsAndRedacts(t *testing.T) {
	cmds := []gitx.CommandRecord{
		{Args: []string{"fetch"}, Exit: 0},
		{Args: []string{"clone", "--", "ssh://CANARYHOST:22/CANARYORG/_git/r", "r"}, Exit: 128, Err: "Host key verification failed. git@CANARYHOST:v3/CANARYORG/r token ghp_" + strings.Repeat("a", 36)},
	}
	ev := []store.Event{{Level: "info", Code: "ok", Message: "fine"}, {Level: "error", Code: "clone", Message: "failed https://CANARYHOST/x"}}
	raw := reportURL(Info{Version: "1.2.3", GitVersion: "2.50"}, cmds, ev)

	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" || u.Query().Get("template") != "bug_report.yml" || u.Query().Get("version") != "1.2.3" {
		t.Fatalf("bad link %q: %v", raw, err)
	}
	what := u.Query().Get("what")
	for _, bad := range []string{"CANARYHOST", "CANARYORG", "ghp_"} {
		if strings.Contains(what, bad) {
			t.Errorf("leaks %s:\n%s", bad, what)
		}
	}
	for _, want := range []string{"Host key verification failed", "exit 128", "ssh://<redacted>", "error clone", "GIT_SSH_COMMAND="} {
		if !strings.Contains(what, want) {
			t.Errorf("missing %q:\n%s", want, what)
		}
	}
	if strings.Contains(what, "fine") {
		t.Error("info events should be left out")
	}
}

func TestReportURL_HugeError_Truncated(t *testing.T) {
	cmds := []gitx.CommandRecord{{Args: []string{"x"}, Exit: 1, Err: strings.Repeat("é", 50000)}}
	if got := reportURL(Info{}, cmds, nil); len(got) > 8000 {
		t.Errorf("link is %d bytes", len(got))
	}
}
