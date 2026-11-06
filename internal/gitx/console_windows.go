// SPDX-License-Identifier: Apache-2.0

//go:build windows

package gitx

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hideConsole gives git a hidden console that ssh and helpers inherit; the detached daemon has none, so each child would open a visible window.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}
