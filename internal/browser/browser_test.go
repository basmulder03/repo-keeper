// SPDX-License-Identifier: Apache-2.0

package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpen_RefusesNonLoopback(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1/x", "http://example.com/", "file:///etc/passwd", "javascript:alert(1)", "http://localhost/", "::bad", "http://[::1"} {
		if err := Open(u); err == nil {
			t.Errorf("%q must be refused", u)
		}
	}
}

func TestOpen_LaunchesOpenerWithTheValidatedURLOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses a fake xdg-open")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\n"
	if err := os.WriteFile(filepath.Join(dir, "xdg-open"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := Open("http://127.0.0.1:7878/login?code=abc"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	if strings.TrimSpace(string(got)) != "http://127.0.0.1:7878/login?code=abc" {
		t.Fatalf("opener got %q", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "xdg-open"), []byte("#!/bin/sh\nexit 3\n"), 0o700) //nolint:gosec // test fixture
	if err := Open("http://127.0.0.1:1/"); err == nil {
		t.Fatal("a failing opener must be reported")
	}
}
