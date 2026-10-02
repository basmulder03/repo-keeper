// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// detach makes the child its own session leader so it survives the terminal and the parent.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// reexecSelf replaces this process image (same PID, so a service manager keeps supervising it).
func reexecSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// #nosec G204 G702 -- re-executes this very binary with its own arguments
	err = syscall.Exec(exe, os.Args, os.Environ()) //nolint:gosec // see #nosec above
	return fmt.Errorf("re-exec %s: %w", exe, err)
}
