// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func FuzzReadAll_ArbitraryFileContent_NeverPanics(f *testing.F) {
	for _, s := range []string{"{\"action\":\"deleted\"}\n", "garbage\n\n{", "\x00\xff", strings.Repeat("a", 2<<20)} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "a.jsonl")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		j, _ := OpenFile(p)
		_, _ = j.ReadAll()
	})
}

func TestOpenFile_UnusableDirectory_Fails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if _, err := OpenFile(filepath.Join(blocker, "sub", "audit.jsonl")); err == nil {
		t.Fatal("a path under a regular file must fail")
	}
}

func TestFile_Append_FailsWhenTargetIsADirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(t.Context(), Entry{Action: Deleted}); err == nil {
		t.Fatal("append must report the failure so cleanup fails closed")
	}
	if _, err := f.ReadAll(); err == nil {
		t.Fatal("reading a directory must error")
	}
}

func TestMem_AppendRecordsAndCanFail(t *testing.T) {
	var m Mem
	if err := m.Append(t.Context(), Entry{Action: Restored}); err != nil || len(m.Entries) != 1 {
		t.Fatalf("entries=%v err=%v", m.Entries, err)
	}
	m.Err = os.ErrPermission
	if err := m.Append(t.Context(), Entry{}); err == nil || len(m.Entries) != 1 {
		t.Fatal("injected failure must surface and not record")
	}
}
