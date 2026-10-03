// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/tray"
)

// TestMain lets `start` spawn a real detached child: the test binary re-executes itself as the CLI.
func TestMain(m *testing.M) {
	if os.Getenv("REPO_KEEPER_TEST_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// isolatedEnv points every per-user location (and git) at temp dirs and makes children run as the CLI.
func isolatedEnv(t *testing.T) (cfg, stateDir, runtimeDir string) {
	t.Helper()
	root := t.TempDir()
	runtimeDir = filepath.Join(root, "run")
	_ = os.MkdirAll(runtimeDir, 0o700)
	for k, v := range map[string]string{
		"HOME": root, "XDG_CONFIG_HOME": filepath.Join(root, "cfg"), "XDG_STATE_HOME": filepath.Join(root, "state"),
		"XDG_RUNTIME_DIR": runtimeDir, "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"REPO_KEEPER_TEST_AS_MAIN": "1",
	} {
		t.Setenv(k, v)
	}
	cfg = filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfg, []byte("[cleanup]\nmode = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, filepath.Join(root, "state", "repo-keeper"), filepath.Join(runtimeDir, "repo-keeper")
}

func TestLifecycle_StartDetached_Status_Restart_Stop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics")
	}
	e := gitxtest.New(t)
	cfg, stateDir, runtimeDir := isolatedEnv(t)
	a, out, errb := newApp(e)
	ctx := context.Background()
	flags := []string{"--config", cfg, "--state-dir", stateDir, "--runtime-dir", runtimeDir}
	t.Cleanup(func() { newAppQuiet(e).run(ctx, append([]string{"stop"}, flags...)) })

	if code := a.run(ctx, append([]string{"start"}, flags...)); code != 0 || !strings.Contains(out.String(), "started (pid") {
		t.Fatalf("start: code=%d out=%s err=%s", code, out, errb)
	}
	c := &tray.Client{RuntimeDir: runtimeDir}
	st, err := c.Status(ctx)
	if err != nil || st.Version != version {
		t.Fatalf("the detached daemon must answer on its API: %+v %v", st, err)
	}
	if fi, err := os.Stat(filepath.Join(stateDir, "daemon.log")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("log file missing or not private: %v %v", fi, err)
	}
	before, _ := c.Runtime()

	out.Reset()
	if code := a.run(ctx, append([]string{"start"}, flags...)); code != 0 || !strings.Contains(out.String(), "already running") {
		t.Fatalf("second start must be a no-op: code=%d out=%s", code, out)
	}

	out.Reset()
	if code := a.run(ctx, append([]string{"restart"}, flags...)); code != 0 || !strings.Contains(out.String(), "restarted") {
		t.Fatalf("restart: code=%d out=%s err=%s", code, out, errb)
	}
	after, err := c.Runtime()
	if err != nil || after.Control == before.Control || after.PID != before.PID {
		t.Fatalf("restart must re-execute in place: new control token, same pid (before=%+v after=%+v err=%v)", before, after, err)
	}

	out.Reset()
	if code := a.run(ctx, append([]string{"stop"}, flags...)); code != 0 || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("stop: code=%d out=%s err=%s", code, out, errb)
	}
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("daemon still answering after stop")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "ui.json")); err == nil {
		t.Fatal("runtime file left behind after a graceful stop")
	}
	out.Reset()
	if code := a.run(ctx, append([]string{"stop"}, flags...)); code != 0 || !strings.Contains(out.String(), "not running") {
		t.Fatalf("stop when stopped: code=%d out=%s", code, out)
	}
	out.Reset()
	if code := a.run(ctx, append([]string{"restart"}, flags...)); code != 0 || !strings.Contains(out.String(), "started") {
		t.Fatalf("restart of a stopped daemon starts it: code=%d out=%s err=%s", code, out, errb)
	}
}

func newAppQuiet(e *gitxtest.Env) *app { a, _, _ := newApp(e); return a }

func TestLifecycle_Start_ReportsEarlyFailureWithLogTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics")
	}
	e := gitxtest.New(t)
	cfg, stateDir, runtimeDir := isolatedEnv(t)
	_ = os.WriteFile(cfg, []byte("[general]\ninterval = \"1m\"\n"), 0o600) // invalid: below the floor
	a, _, errb := newApp(e)
	code := a.run(context.Background(), []string{"start", "--config", cfg, "--state-dir", stateDir, "--runtime-dir", runtimeDir})
	if code != 1 || !strings.Contains(errb.String(), "exited right away") || !strings.Contains(errb.String(), "minimum") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestDaemon_RestartRequest_StopsThenReexecs_StopRequestDoesNot(t *testing.T) {
	e := gitxtest.New(t)
	cfg, stateDir, runtimeDir := isolatedEnv(t)
	t.Setenv("REPO_KEEPER_TEST_AS_MAIN", "") // this daemon runs in-process
	for _, restart := range []bool{true, false} {
		a, _, errb := newApp(e)
		var reexecs atomic.Int32
		a.reexec = func() error { reexecs.Add(1); return nil }
		done := make(chan int, 1)
		go func() {
			done <- a.run(context.Background(), []string{"daemon", "--config", cfg, "--state-dir", stateDir, "--log-level", "error"})
		}()
		c := &tray.Client{RuntimeDir: runtimeDir}
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := c.Status(context.Background()); err == nil {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		if err := c.Shutdown(context.Background(), restart); err != nil {
			t.Fatalf("restart=%v: %v", restart, err)
		}
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("restart=%v: exit %d err=%s", restart, code, errb)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("restart=%v: daemon did not stop", restart)
		}
		if got := reexecs.Load(); (restart && got != 1) || (!restart && got != 0) {
			t.Fatalf("restart=%v: reexec called %d times", restart, got)
		}
	}
}

func TestLifecycle_NoUI_StartIsReady_SecondStartAndStopAndRestartWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics")
	}
	e := gitxtest.New(t)
	cfg, stateDir, runtimeDir := isolatedEnv(t)
	a, out, errb := newApp(e)
	ctx := context.Background()
	flags := []string{"--config", cfg, "--state-dir", stateDir, "--runtime-dir", runtimeDir, "--no-ui"}
	t.Cleanup(func() { newAppQuiet(e).run(ctx, append([]string{"stop"}, flags...)) })

	if code := a.run(ctx, append([]string{"start"}, flags...)); code != 0 || !strings.Contains(out.String(), "started (pid") {
		t.Fatalf("start without a UI must report ready: code=%d out=%s err=%s", code, out, errb)
	}
	out.Reset()
	if code := a.run(ctx, append([]string{"start"}, flags...)); code != 0 || !strings.Contains(out.String(), "already running") {
		t.Fatalf("second start must say already running, not fail: code=%d out=%s err=%s", code, out, errb)
	}
	out.Reset()
	if code := a.run(ctx, append([]string{"restart"}, flags...)); code != 0 || !strings.Contains(out.String(), "started (pid") {
		t.Fatalf("restart: code=%d out=%s err=%s", code, out, errb)
	}
	out.Reset()
	if code := a.run(ctx, append([]string{"stop"}, flags...)); code != 0 || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("stop without a UI: code=%d out=%s err=%s", code, out, errb)
	}
	out.Reset()
	if code := a.run(ctx, append([]string{"stop"}, flags...)); code != 0 || !strings.Contains(out.String(), "not running") {
		t.Fatalf("stop when nothing runs: code=%d out=%s err=%s", code, out, errb)
	}
}
