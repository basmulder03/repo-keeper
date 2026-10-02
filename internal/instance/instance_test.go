// SPDX-License-Identifier: Apache-2.0

package instance

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquire_SecondFailsUntilRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run", "daemon.lock")
	l1, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(p); !errors.Is(err, ErrRunning) {
		t.Fatalf("err=%v", err)
	}
	l1.Release()
	l2, err := Acquire(p)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
	l2.Release() // idempotent
}
