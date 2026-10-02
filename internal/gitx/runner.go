// SPDX-License-Identifier: Apache-2.0

// Package gitx is the only place repo-keeper executes git: hooks off, scrubbed env, no shell.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/obs"
)

// MinVersion is the oldest supported git (see ADR-0013).
var MinVersion = Version{2, 34, 0}

const (
	maxOutput      = 32 << 20
	defaultTimeout = 10 * time.Minute
)

// Version is a parsed git version.
type Version struct{ Major, Minor, Patch int }

// String implements fmt.Stringer.
func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// AtLeast reports whether v >= o.
func (v Version) AtLeast(o Version) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Patch >= o.Patch
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion extracts the version from `git version` output.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("gitx: no version in %q", s)
	}
	n := func(x string) int { i, _ := strconv.Atoi(x); return i }
	return Version{n(m[1]), n(m[2]), n(m[3])}, nil
}

// Error is a failed git invocation; Stderr is scrubbed of token shapes.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", obs.Scrub(strings.Join(e.Args, " ")), e.ExitCode, e.Stderr)
}

// Runner executes git with a hardened environment.
type Runner struct {
	// Bin is the git executable (absolute path preferred).
	Bin string
	// Env entries (KEY=VALUE) are applied last; tests use this to isolate HOME and config.
	Env []string
	// Timeout bounds each invocation when the context has no deadline.
	Timeout time.Duration
}

// New locates git on PATH and verifies the minimum version.
func New() (*Runner, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("gitx: git not found on PATH: %w", err)
	}
	r := &Runner{Bin: bin}
	v, err := r.Version(context.Background())
	if err != nil {
		return nil, err
	}
	if !v.AtLeast(MinVersion) {
		return nil, fmt.Errorf("gitx: git %s is older than required %s", v, MinVersion)
	}
	return r, nil
}

// Version returns the installed git version.
func (r *Runner) Version(ctx context.Context) (Version, error) {
	out, err := r.Run(ctx, "", "version")
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(out)
}

// allowedEnv is the allow-list of inherited variables; everything else (tokens, GIT_DIR, ...) is dropped.
var allowedEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "LANG": true,
	"SSH_AUTH_SOCK": true, "XDG_CONFIG_HOME": true, "XDG_RUNTIME_DIR": true, "XDG_DATA_HOME": true,
	"SYSTEMROOT": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "TEMP": true, "TMP": true,
	"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
}

// hardening applies with the highest config precedence, beating repo-local config.
func hardening() []string {
	cfg := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"protocol.ext.allow", "never"},
		{"submodule.recurse", "false"},
		{"fetch.recurseSubmodules", "false"},
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(cfg))}
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// buildEnv returns the child environment: allow-listed base, fixed safety vars, then overrides.
func buildEnv(base, overrides []string) []string {
	var env []string
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && allowedEnv[strings.ToUpper(k)] {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0", // never block on a credential prompt
		"GCM_INTERACTIVE=never",
		"GIT_OPTIONAL_LOCKS=0", // read-only commands must not take index locks
		"LC_ALL=C",
	)
	env = append(env, hardening()...)
	return append(env, overrides...)
}

// Run executes `git args...` in dir (empty = current) and returns stdout.
func (r *Runner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		t := r.Timeout
		if t == 0 {
			t = defaultTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	// #nosec G204 -- the one sanctioned exec site; no shell, args built by gitx
	cmd := exec.CommandContext(ctx, r.Bin, args...) //nolint:forbidigo,gosec // see #nosec above
	cmd.Dir = dir
	cmd.Env = buildEnv(os.Environ(), r.Env)
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		code := -1
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("gitx: git %s: %w", args[0], ctx.Err())
		}
		return stdout.String(), &Error{Args: args, ExitCode: code, Stderr: obs.Scrub(strings.TrimSpace(stderr.String()))}
	}
	return stdout.String(), nil
}

// ExitCode returns the git exit code carried by err, or -1.
func ExitCode(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.ExitCode
	}
	return -1
}

// limitedBuffer caps captured output to avoid unbounded memory use.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxOutput {
		return 0, errors.New("gitx: output limit exceeded")
	}
	return b.Buffer.Write(p)
}
