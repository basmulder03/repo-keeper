// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMain lets Windows runs keep POSIX-style fixture paths ("/r"); the validation rule itself is covered by the relative-path cases.
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		isAbs = func(p string) bool { return filepath.IsAbs(p) || strings.HasPrefix(p, "/") }
	}
	os.Exit(m.Run())
}
