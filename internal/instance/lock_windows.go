// SPDX-License-Identifier: Apache-2.0

//go:build windows

package instance

import (
	"errors"
	"os"
	"syscall"
)

// lockFile opens the file with no sharing: a second open fails with a sharing violation until the handle closes.
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, syscall.Errno(32)) { // ERROR_SHARING_VIOLATION
			return nil, ErrRunning
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
