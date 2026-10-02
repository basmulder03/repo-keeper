// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFake_Advance_FiresOnlyDueWaiters(t *testing.T) {
	f := NewFake(t0)
	short := f.After(time.Minute)
	long := f.After(time.Hour)

	f.Advance(time.Minute)

	select {
	case got := <-short:
		if !got.Equal(t0.Add(time.Minute)) {
			t.Fatalf("fired at %v", got)
		}
	default:
		t.Fatal("short waiter should have fired")
	}
	select {
	case <-long:
		t.Fatal("long waiter fired early")
	default:
	}
}

func TestFake_After_NonPositiveFiresImmediately(t *testing.T) {
	f := NewFake(t0)
	select {
	case <-f.After(0):
	default:
		t.Fatal("expected immediate fire")
	}
}

func TestFake_Now_ReflectsAdvance(t *testing.T) {
	f := NewFake(t0)
	f.Advance(90 * time.Second)
	if want := t0.Add(90 * time.Second); !f.Now().Equal(want) {
		t.Fatalf("Now() = %v, want %v", f.Now(), want)
	}
}

func TestReal_Now_IsRecent(t *testing.T) {
	if d := time.Since(Real{}.Now()); d < 0 || d > time.Minute {
		t.Fatalf("unexpected skew %v", d)
	}
}
