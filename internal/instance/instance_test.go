// SPDX-License-Identifier: Apache-2.0

package instance

import (
	"errors"
	"os"
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

func TestHolder_ReportsRunningAndPid_OnlyWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.lock")
	if _, running := Holder(path); running {
		t.Fatal("no lock yet")
	}
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, running := Holder(path)
	if !running || (pid != 0 && pid != os.Getpid()) {
		t.Fatalf("pid=%d running=%v", pid, running)
	}
	l.Release()
	if _, running := Holder(path); running {
		t.Fatal("released lock still reported as held")
	}
}
