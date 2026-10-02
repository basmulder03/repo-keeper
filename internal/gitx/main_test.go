// SPDX-License-Identifier: Apache-2.0

package gitx_test

import (
	"os"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/askpass"
)

// The test binary doubles as the askpass helper, exactly like the real binary does.
func TestMain(m *testing.M) {
	if os.Getenv(askpass.EnvMarker) == "1" {
		os.Exit(askpass.Run(os.Args[1:], os.Getenv, os.Stdout))
	}
	os.Exit(m.Run())
}
