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

func TestRuntimeDir_PrefersXDGRuntimeDir(t *testing.T) {
	x := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", x)
	if got, err := RuntimeDir(); err != nil || got != filepath.Join(x, "repo-keeper") {
		t.Fatalf("got=%q err=%v", got, err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "relative/dir") // relative values are ignored, never trusted
	t.Setenv("XDG_STATE_HOME", x)
	if got, _ := RuntimeDir(); got != filepath.Join(x, "repo-keeper") {
		t.Fatalf("fallback should be the state dir, got %q", got)
	}
}

func TestStateDir_RelativeXDGIgnored(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux layout")
	}
	h := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "relative")
	t.Setenv("HOME", h)
	if got, _ := StateDir(); got != filepath.Join(h, ".local", "state", "repo-keeper") {
		t.Fatalf("got=%q", got)
	}
}

func TestConfigPath_EndsWithAppConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := ConfigPath()
	if err != nil || filepath.Base(got) != "config.toml" || filepath.Base(filepath.Dir(got)) != "repo-keeper" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}
