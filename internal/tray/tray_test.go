// SPDX-License-Identifier: Apache-2.0

package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/control"
)

func TestSummarize(t *testing.T) {
	tests := []struct {
		name      string
		st        *control.Status
		state     State
		line      string
		pauseText string
	}{
		{"daemon down", nil, Down, "Daemon not running", "Pause syncing"},
		{"all good", &control.Status{Repos: 12, UpToDate: 12}, OK, "All 12 repositories up to date", "Pause syncing"},
		{"single repo", &control.Status{Repos: 1, UpToDate: 1}, OK, "All 1 repository up to date", "Pause syncing"},
		{"pending", &control.Status{Repos: 5, UpToDate: 3, Pending: 2}, OK, "3 up to date · 2 pending", "Pause syncing"},
		{"empty", &control.Status{}, OK, "No repositories yet", "Pause syncing"},
		{"attention", &control.Status{Repos: 5, UpToDate: 3, NeedAttention: 2}, Attention, "2 repositories need attention", "Pause syncing"},
		{"one attention", &control.Status{Repos: 5, NeedAttention: 1}, Attention, "1 repository needs attention", "Pause syncing"},
		{"account only", &control.Status{Repos: 5, UpToDate: 5, Accounts: 2, AccountsAttention: 1}, Attention, "1 account needs attention", "Pause syncing"},
		{"both", &control.Status{Repos: 5, NeedAttention: 2, AccountsAttention: 1}, Attention, "2 repositories need attention · 1 account", "Pause syncing"},
		{"paused", &control.Status{Repos: 4, UpToDate: 4, Paused: true}, Paused, "Paused · 4 repositories", "Resume syncing"},
		{"attention outranks pause", &control.Status{Repos: 4, NeedAttention: 1, Paused: true}, Attention, "1 repository needs attention", "Resume syncing"},
	}
	for _, tc := range tests {
		v := Summarize(tc.st)
		if v.State != tc.state || v.Line != tc.line || v.PauseLabel != tc.pauseText || !strings.Contains(strings.ToLower(v.Tooltip), strings.ToLower(tc.line)) {
			t.Errorf("%s: %+v", tc.name, v)
		}
	}
}

func TestIcon_ValidDistinctAndCached(t *testing.T) {
	centers := map[State]color.Color{}
	for _, s := range []State{Down, OK, Attention, Paused} {
		b := Icon(s)
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if img.Bounds() != image.Rect(0, 0, 64, 64) {
			t.Fatalf("%s: bounds %v", s, img.Bounds())
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Fatalf("%s: corners must be transparent", s)
		}
		if _, _, _, a := img.At(32, 8).RGBA(); a == 0 {
			t.Fatalf("%s: circle body missing", s)
		}
		centers[s] = img.At(32, 8)
		if &Icon(s)[0] != &b[0] {
			t.Errorf("%s: icon not cached", s)
		}
	}
	seen := map[[4]uint32]bool{}
	for _, c := range centers {
		r, g, b, a := c.RGBA()
		seen[[4]uint32{r, g, b, a}] = true
	}
	if len(seen) != 4 {
		t.Fatalf("states must be visually distinct, got %d colours", len(seen))
	}
}

func daemon(t *testing.T, handler http.HandlerFunc) (dir string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir = t.TempDir()
	b, _ := json.Marshal(control.RuntimeFile{Addr: strings.TrimPrefix(srv.URL, "http://"), Control: "tok", PID: 1})
	if err := os.WriteFile(filepath.Join(dir, "ui.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestClient_StatusAndActions(t *testing.T) {
	var paused, syncs atomic.Int32
	dir := daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/status":
			_ = json.NewEncoder(w).Encode(control.Status{Repos: 3, UpToDate: 2, NeedAttention: 1, Version: "v"})
		case "POST /api/sync-all":
			syncs.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "POST /api/pause":
			paused.Store(1)
			w.WriteHeader(http.StatusNoContent)
		case "POST /api/resume":
			paused.Store(2)
			w.WriteHeader(http.StatusNoContent)
		case "POST /api/login-url":
			_, _ = w.Write([]byte(`{"url":"http://127.0.0.1:1/login?code=x"}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := &Client{RuntimeDir: dir}
	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil || st.Repos != 3 || st.NeedAttention != 1 {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	if err := c.SyncAll(ctx); err != nil || syncs.Load() != 1 {
		t.Fatalf("sync-all: %v", err)
	}
	if err := c.SetPaused(ctx, true); err != nil || paused.Load() != 1 {
		t.Fatalf("pause: %v", err)
	}
	if err := c.SetPaused(ctx, false); err != nil || paused.Load() != 2 {
		t.Fatalf("resume: %v", err)
	}
	if u, err := c.LoginURL(ctx); err != nil || !strings.Contains(u, "code=x") {
		t.Fatalf("login: %q %v", u, err)
	}
}

func TestClient_Failures(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Client{RuntimeDir: t.TempDir()}).Status(ctx); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no runtime file: %v", err)
	}
	dead := t.TempDir()
	b, _ := json.Marshal(control.RuntimeFile{Addr: "127.0.0.1:1", Control: "tok"})
	_ = os.WriteFile(filepath.Join(dead, "ui.json"), b, 0o600)
	if _, err := (&Client{RuntimeDir: dead}).Status(ctx); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stale file: %v", err)
	}
	remote := t.TempDir()
	b, _ = json.Marshal(control.RuntimeFile{Addr: "203.0.113.9:7878", Control: "tok"})
	_ = os.WriteFile(filepath.Join(remote, "ui.json"), b, 0o600)
	if _, err := (&Client{RuntimeDir: remote}).Status(ctx); err == nil || errors.Is(err, ErrNotRunning) {
		t.Fatalf("non-loopback address must be refused explicitly: %v", err)
	}
	bad := daemon(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", http.StatusUnauthorized) })
	c := &Client{RuntimeDir: bad}
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("401 must be an error")
	}
	if err := c.SyncAll(ctx); err == nil {
		t.Fatal("401 must be an error")
	}
	garbage := daemon(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) })
	if _, err := (&Client{RuntimeDir: garbage}).Status(ctx); err == nil {
		t.Fatal("garbage payload must be an error")
	}
	if _, err := (&Client{RuntimeDir: garbage}).LoginURL(ctx); err == nil {
		t.Fatal("garbage login-url payload must be an error")
	}
}

func TestClient_PicksUpRestartedDaemon(t *testing.T) {
	c := &Client{RuntimeDir: t.TempDir()}
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatal("expected not running")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(control.Status{Repos: 1}) }))
	defer srv.Close()
	b, _ := json.Marshal(control.RuntimeFile{Addr: strings.TrimPrefix(srv.URL, "http://"), Control: "t"})
	_ = os.WriteFile(filepath.Join(c.RuntimeDir, "ui.json"), b, 0o600)
	if st, err := c.Status(context.Background()); err != nil || st.Repos != 1 {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}
