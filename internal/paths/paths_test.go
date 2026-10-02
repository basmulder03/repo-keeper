// SPDX-License-Identifier: Apache-2.0

package paths

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestStateDir_XDGWins(t *testing.T) {
	x := t.TempDir()
	t.Setenv("XDG_STATE_HOME", x)
	got, err := StateDir()
	if err != nil || got != filepath.Join(x, "repo-keeper") {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if j, _ := JournalPath(); j != filepath.Join(x, "repo-keeper", "audit.jsonl") {
		t.Fatalf("journal=%q", j)
	}
}

func TestStateDir_FallbackUnderHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux layout")
	}
	h := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", h)
	if got, _ := StateDir(); got != filepath.Join(h, ".local", "state", "repo-keeper") {
		t.Fatalf("got=%q", got)
	}
}
