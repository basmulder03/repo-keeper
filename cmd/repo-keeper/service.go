// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

const (
	serviceName  = "repo-keeper"
	launchdLabel = "io.github.basmulder03.repo-keeper"
)

// runner runs a fixed system tool (systemctl, launchctl, schtasks); a seam so tests never touch the real service manager.
type runner func(ctx context.Context, name string, args ...string) error

func execRunner(ctx context.Context, name string, args ...string) error {
	// #nosec G204 -- fixed tool names and arguments built by this file, never user input
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:forbidigo,gosec // sanctioned: per-user service registration via the official OS tool
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// svc describes one per-user autostart registration on one OS.
type svc struct {
	goos, exe, home, cfgDir string
	uid                     int
	username                string // Windows: DOMAIN\user the task runs as
	run                     runner
}

func newSvc() (*svc, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cfgDir, _ := os.UserConfigDir()
	s := &svc{goos: runtime.GOOS, exe: exe, home: home, cfgDir: cfgDir, uid: os.Getuid(), run: execRunner}
	if u, err := user.Current(); err == nil {
		s.username = u.Username
	}
	return s, nil
}

func (s *svc) unitPath() string {
	return filepath.Join(s.cfgDir, "systemd", "user", serviceName+".service")
}
func (s *svc) plistPath() string {
	return filepath.Join(s.home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func xmlEsc(v string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

func (s *svc) unit() string {
	return "[Unit]\nDescription=repo-keeper: keep local clones in sync\nAfter=network-online.target\n\n" +
		"[Service]\nExecStart=\"" + strings.ReplaceAll(s.exe, "%", "%%") + "\" daemon\nRestart=on-failure\nRestartSec=30\nUMask=0077\nNoNewPrivileges=yes\n\n" +
		"[Install]\nWantedBy=default.target\n"
}

func (s *svc) plist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + launchdLabel + `</string>
<key>ProgramArguments</key><array><string>` + xmlEsc(s.exe) + `</string><string>daemon</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ProcessType</key><string>Background</string>
</dict></plist>
`
}

// taskXML uses a logon trigger bound to the current user: unlike `schtasks /sc onlogon`, importing XML needs no admin rights.
// It runs `start`, which launches the daemon detached and hidden, so no console window stays open.
func (s *svc) taskXML() string {
	u := xmlEsc(s.username)
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
<Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + u + `</UserId></LogonTrigger></Triggers>
<Principals><Principal id="Author"><UserId>` + u + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
<Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
<Actions Context="Author"><Exec><Command>` + xmlEsc(s.exe) + `</Command><Arguments>start</Arguments></Exec></Actions>
</Task>
`
}

func utf16le(v string) []byte {
	u := utf16.Encode([]rune(v))
	b := make([]byte, 0, 2+2*len(u))
	b = append(b, 0xFF, 0xFE) // BOM; schtasks rejects UTF-8 task files
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return b
}

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (s *svc) install(ctx context.Context) (string, error) {
	switch s.goos {
	case "linux":
		if err := writePrivate(s.unitPath(), []byte(s.unit())); err != nil {
			return "", err
		}
		if err := s.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return "", err
		}
		if err := s.run(ctx, "systemctl", "--user", "enable", "--now", serviceName+".service"); err != nil {
			return "", err
		}
		return "enabled and started the systemd user unit " + s.unitPath(), nil
	case "darwin":
		if err := writePrivate(s.plistPath(), []byte(s.plist())); err != nil {
			return "", err
		}
		domain := fmt.Sprintf("gui/%d", s.uid)
		_ = s.run(ctx, "launchctl", "bootout", domain+"/"+launchdLabel) // not loaded yet is fine
		if err := s.run(ctx, "launchctl", "bootstrap", domain, s.plistPath()); err != nil {
			return "", err
		}
		return "registered the LaunchAgent " + s.plistPath() + " (starts now and at every login)", nil
	case "windows":
		f, err := os.CreateTemp("", "repo-keeper-task-*.xml")
		if err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		_, werr := f.Write(utf16le(s.taskXML()))
		if err := errors.Join(werr, f.Close()); err != nil {
			return "", fmt.Errorf("write task file: %w", err)
		}
		if err := s.run(ctx, "schtasks", "/Create", "/TN", serviceName, "/XML", f.Name(), "/F"); err != nil {
			return "", err
		}
		return "registered the Task Scheduler task \"" + serviceName + "\" (starts at your next logon; start it now with: repo-keeper start)", nil
	}
	return "", fmt.Errorf("autostart is not supported on %s", s.goos)
}

func (s *svc) uninstall(ctx context.Context) (string, error) {
	switch s.goos {
	case "linux":
		_ = s.run(ctx, "systemctl", "--user", "disable", "--now", serviceName+".service") // may not be enabled
		if err := os.Remove(s.unitPath()); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		_ = s.run(ctx, "systemctl", "--user", "daemon-reload")
		return "autostart removed", nil
	case "darwin":
		_ = s.run(ctx, "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", s.uid, launchdLabel))
		if err := os.Remove(s.plistPath()); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return "autostart removed", nil
	case "windows":
		if err := s.run(ctx, "schtasks", "/Delete", "/TN", serviceName, "/F"); err != nil {
			return "", err
		}
		return "autostart removed", nil
	}
	return "", fmt.Errorf("autostart is not supported on %s", s.goos)
}

// state reports whether autostart is registered, without starting or changing anything.
func (s *svc) state(ctx context.Context) string {
	switch s.goos {
	case "linux":
		// `systemctl --user enable` (and Home Manager) links the unit into a *.wants dir; reading it avoids running systemctl
		if m, _ := filepath.Glob(filepath.Join(s.cfgDir, "systemd", "user", "*.wants", serviceName+".service")); len(m) > 0 {
			return "enabled (systemd user unit)"
		}
		return "disabled (enable with: repo-keeper install-service)"
	case "darwin":
		if _, err := os.Stat(s.plistPath()); err == nil {
			return "enabled (LaunchAgent)"
		}
		return "disabled (enable with: repo-keeper install-service)"
	case "windows":
		if s.run(ctx, "schtasks", "/Query", "/TN", serviceName) == nil {
			return "enabled (Task Scheduler)"
		}
		return "disabled (enable with: repo-keeper install-service)"
	}
	return "unknown (not detected on " + s.goos + ")"
}

func (a *app) cmdService(ctx context.Context, name string, args []string) int {
	fs := a.newFlagSet(name)
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := newSvc()
	if err == nil {
		var msg string
		if name == "install-service" {
			msg, err = s.install(ctx)
		} else {
			msg, err = s.uninstall(ctx)
		}
		if err == nil {
			a.printf("%s\n", msg)
			return 0
		}
	}
	_, _ = fmt.Fprintln(a.err, name+":", err)
	return 1
}
