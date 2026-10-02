// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/gitxtest"
	"github.com/basmulder03/repo-keeper/internal/ui"
)

func runtimeFile(t *testing.T, addr, control string) string {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.Marshal(ui.RuntimeFile{Addr: addr, Control: control, PID: 1, Started: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, "ui.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUI_Command_PrintsAndOpensOneTimeURL(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/api/login-url" || gotAuth != "Bearer s3cret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"url":"http://` + r.Host + `/login?code=abc"}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	e := gitxtest.New(t)

	a, out, errb := newApp(e)
	if code := a.run(context.Background(), []string{"ui", "--print-url", "--runtime-dir", runtimeFile(t, addr, "s3cret")}); code != 0 || strings.TrimSpace(out.String()) != "http://"+addr+"/login?code=abc" {
		t.Fatalf("print: code=%d out=%q err=%s", code, out, errb)
	}

	var opened string
	a2, out2, _ := newApp(e)
	a2.openBrowser = func(u string) error { opened = u; return nil }
	if code := a2.run(context.Background(), []string{"ui", "--runtime-dir", runtimeFile(t, addr, "s3cret")}); code != 0 || opened != "http://"+addr+"/login?code=abc" || !strings.Contains(out2.String(), "Opened") {
		t.Fatalf("open: code=%d opened=%q out=%s", code, opened, out2)
	}
	if strings.Contains(out2.String(), "code=abc") {
		t.Fatal("the one-time link must not be echoed when the browser opened")
	}

	a3, out3, _ := newApp(e)
	a3.openBrowser = func(string) error { return os.ErrNotExist }
	if code := a3.run(context.Background(), []string{"ui", "--runtime-dir", runtimeFile(t, addr, "s3cret")}); code != 0 || !strings.Contains(out3.String(), "code=abc") {
		t.Fatalf("fallback must print the link: code=%d out=%s", code, out3)
	}
}

func TestUI_Command_Failures(t *testing.T) {
	e := gitxtest.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for name, tc := range map[string]struct{ dir, want string }{
		"no daemon":      {t.TempDir(), "not running"},
		"wrong token":    {runtimeFile(t, addr, "bad"), "refused"},
		"dead address":   {runtimeFile(t, "127.0.0.1:1", "x"), "does not answer"},
		"remote address": {runtimeFile(t, "203.0.113.5:7878", "x"), "non-loopback"},
		"no control":     {runtimeFile(t, addr, ""), "unreadable"},
	} {
		a, _, errb := newApp(e)
		if code := a.run(context.Background(), []string{"ui", "--print-url", "--runtime-dir", tc.dir}); code != 1 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%s: code=%d err=%s", name, code, errb)
		}
	}
}
