// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package gitx

import "os/exec"

// hideConsole is a no-op outside Windows.
func hideConsole(*exec.Cmd) {}
