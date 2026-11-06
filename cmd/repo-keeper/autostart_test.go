// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type calls struct {
	got  []string
	fail bool
}

func (c *calls) run(_ context.Context, name string, args ...string) error {
	c.got = append(c.got, name+" "+strings.Join(args, " "))
	if c.fail {
		return errors.New("boom")
	}
	return nil
}

func testSvc(t *testing.T, goos string, c *calls) *svc {
	t.Helper()
	h := t.TempDir()
	return &svc{goos: goos, exe: `/opt/my app/repo-keeper`, home: h, cfgDir: filepath.Join(h, ".config"), uid: 501, username: `HOST\bas`, run: c.run}
}

func TestSvc_InstallUninstall_PerOS(t *testing.T) {
	for _, tc := range []struct {
		goos, file, wantCall string
	}{
		{"linux", "unit", "systemctl --user enable --now repo-keeper.service"},
		{"darwin", "plist", "launchctl bootstrap gui/501 "},
		{"windows", "", "schtasks /Create /TN repo-keeper /XML "},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			c := &calls{}
			s := testSvc(t, tc.goos, c)
			if _, err := s.install(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(c.got, "\n"), tc.wantCall) {
				t.Errorf("calls %q lack %q", c.got, tc.wantCall)
			}
			var path string
			switch tc.file {
			case "unit":
				path = s.unitPath()
			case "plist":
				path = s.plistPath()
			}
			if path != "" {
				b, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(b), s.exe) {
					t.Errorf("file not written with exe path: %v", err)
				}
			}
			if _, err := s.uninstall(context.Background()); err != nil {
				t.Fatal(err)
			}
			if path != "" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("file not removed: %v", err)
				}
			}
		})
	}
}

func TestSvc_InstallToolFails_ReturnsError(t *testing.T) {
	s := testSvc(t, "windows", &calls{fail: true})
	if _, err := s.install(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestSvc_State_Reported(t *testing.T) {
	ctx := context.Background()
	lin := testSvc(t, "linux", &calls{})
	if got := lin.state(ctx); !strings.HasPrefix(got, "disabled") {
		t.Errorf("linux before: %q", got)
	}
	wants := filepath.Join(lin.cfgDir, "systemd", "user", "default.target.wants")
	if err := os.MkdirAll(wants, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wants, "repo-keeper.service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lin.state(ctx); !strings.HasPrefix(got, "enabled") {
		t.Errorf("linux after: %q", got)
	}
	if got := testSvc(t, "windows", &calls{}).state(ctx); !strings.HasPrefix(got, "enabled") {
		t.Errorf("windows registered: %q", got)
	}
	if got := testSvc(t, "windows", &calls{fail: true}).state(ctx); !strings.HasPrefix(got, "disabled") {
		t.Errorf("windows missing: %q", got)
	}
	mac := testSvc(t, "darwin", &calls{})
	if got := mac.state(ctx); !strings.HasPrefix(got, "disabled") {
		t.Errorf("mac before: %q", got)
	}
	if got := testSvc(t, "plan9", &calls{}).state(ctx); !strings.HasPrefix(got, "unknown") {
		t.Errorf("other: %q", got)
	}
}

func TestSvc_TaskXML_EscapesAndUTF16(t *testing.T) {
	s := testSvc(t, "windows", &calls{})
	s.exe = `C:\a&b\repo-keeper.exe`
	if x := s.taskXML(); !strings.Contains(x, `a&amp;b`) {
		t.Error("exe not escaped")
	}
	if b := utf16le("A"); len(b) != 4 || b[0] != 0xFF || b[2] != 'A' {
		t.Errorf("bad utf16: %v", b)
	}
}
