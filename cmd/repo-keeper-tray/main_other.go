// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

// Command repo-keeper-tray is not available on this platform yet (macOS needs a cgo build of the tray library).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "repo-keeper-tray is not supported on this platform yet; use `repo-keeper ui`.")
	os.Exit(1)
}
