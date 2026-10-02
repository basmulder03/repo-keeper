// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package instance

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(path string) (*os.File, error) {
	// #nosec G304 -- our own lock path
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // see #nosec above
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, err
	}
	return f, nil
}
