// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestFile_AppendReadAll_RoundTripAndPerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "audit.jsonl")
	f, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := Entry{Time: time.Unix(1_700_000_000, 0).UTC(), Repo: "/r", Branch: "feat", SHA: "abc", Action: Deleted, Mode: "auto", Reason: "merged-into-default"}
	if err := f.Append(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadAll()
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("perm=%v", st.Mode().Perm())
		}
	}
}

func TestFile_ReadAll_MissingAndMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	f, _ := OpenFile(p)
	if got, err := f.ReadAll(); err != nil || got != nil {
		t.Fatalf("missing file: %v %v", got, err)
	}
	_ = os.WriteFile(p, []byte("garbage\n{\"action\":\"deleted\"}\n"), 0o600)
	if got, _ := f.ReadAll(); len(got) != 1 || got[0].Action != Deleted {
		t.Fatalf("got=%+v", got)
	}
}

func TestFile_Append_Concurrent(t *testing.T) {
	f, _ := OpenFile(filepath.Join(t.TempDir(), "a.jsonl"))
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.Append(t.Context(), Entry{Action: Blocked}) }()
	}
	wg.Wait()
	if got, _ := f.ReadAll(); len(got) != 20 {
		t.Fatalf("lines=%d", len(got))
	}
}
