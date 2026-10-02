// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newFile(t *testing.T, pass string) *EncryptedFile {
	t.Helper()
	return &EncryptedFile{
		Path:       filepath.Join(t.TempDir(), "sub", "secrets.enc"),
		Passphrase: func() (string, error) { return pass, nil },
		Iterations: minIter, // fast for tests; the default is exercised in TestDefaultIterations
	}
}

func TestEncryptedFile_RoundTrip_NothingPlainOnDisk(t *testing.T) {
	f := newFile(t, "correct horse battery staple")
	if _, err := f.Get("gh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store: %v", err)
	}
	if err := f.Set("account/gh", New(canary)); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("account/gl", New("glpat-second-secret-value")); err != nil {
		t.Fatal(err)
	}
	got, err := f.Get("account/gh")
	if err != nil || got.Reveal() != canary {
		t.Fatalf("got=%v err=%v", got, err)
	}
	raw, _ := os.ReadFile(f.Path)
	for _, leak := range []string{canary, "glpat-second", "account/gh", "correct horse"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Fatalf("file contains %q in clear", leak)
		}
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(f.Path); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	if err := f.Delete("account/gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("account/gh"); !errors.Is(err, ErrNotFound) {
		t.Fatal("not deleted")
	}
	if g, err := f.Get("account/gl"); err != nil || g.Reveal() != "glpat-second-secret-value" {
		t.Fatal("other secret lost by delete")
	}
}

func TestEncryptedFile_WrongPassphrase_FailsAndNeverOverwrites(t *testing.T) {
	f := newFile(t, "right")
	if err := f.Set("a", New("value-a")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f.Path)
	wrong := &EncryptedFile{Path: f.Path, Passphrase: func() (string, error) { return "wrong", nil }, Iterations: minIter}
	if _, err := wrong.Get("a"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("get: %v", err)
	}
	if err := wrong.Set("b", New("value-b")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("set: %v", err)
	}
	if err := wrong.Delete("a"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("delete: %v", err)
	}
	after, _ := os.ReadFile(f.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("a failed write with the wrong passphrase modified the file")
	}
}

func TestEncryptedFile_FreshSaltAndNonceEveryWrite(t *testing.T) {
	f := newFile(t, "p")
	_ = f.Set("a", New("same-value"))
	first, _ := os.ReadFile(f.Path)
	_ = f.Set("a", New("same-value"))
	second, _ := os.ReadFile(f.Path)
	var e1, e2 envelope
	_ = json.Unmarshal(first, &e1)
	_ = json.Unmarshal(second, &e2)
	if e1.Salt == e2.Salt || e1.Nonce == e2.Nonce || e1.Data == e2.Data {
		t.Fatal("salt, nonce and ciphertext must all change on every write")
	}
}

func TestEncryptedFile_TamperingAndDowngradeRefused(t *testing.T) {
	f := newFile(t, "p")
	_ = f.Set("a", New("value"))
	raw, _ := os.ReadFile(f.Path)
	var e envelope
	_ = json.Unmarshal(raw, &e)

	mutate := func(fn func(*envelope)) error {
		c := e
		fn(&c)
		b, _ := json.Marshal(c)
		_ = os.WriteFile(f.Path, b, 0o600)
		_, err := f.Get("a")
		return err
	}
	ct, _ := base64.StdEncoding.DecodeString(e.Data)
	ct[0] ^= 1
	for name, fn := range map[string]func(*envelope){
		"flipped ciphertext bit": func(c *envelope) { c.Data = base64.StdEncoding.EncodeToString(ct) },
		"iteration downgrade":    func(c *envelope) { c.Iter = 1 },
		"absurd iteration count": func(c *envelope) { c.Iter = 1 << 30 },
		"other kdf":              func(c *envelope) { c.KDF = "md5" },
		"future version":         func(c *envelope) { c.Version = 99 },
		"short salt":             func(c *envelope) { c.Salt = base64.StdEncoding.EncodeToString([]byte("x")) },
		"bad nonce length":       func(c *envelope) { c.Nonce = base64.StdEncoding.EncodeToString([]byte("123")) },
		"garbage base64":         func(c *envelope) { c.Data = "!!!" },
	} {
		if err := mutate(fn); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_ = os.WriteFile(f.Path, []byte("not json"), 0o600)
	if _, err := f.Get("a"); err == nil || strings.Contains(err.Error(), "value") {
		t.Fatalf("err=%v", err)
	}
	if err := f.Set("b", New("x")); err == nil {
		t.Fatal("must not overwrite a file it cannot read")
	}
	if b, _ := os.ReadFile(f.Path); string(b) != "not json" {
		t.Fatal("unreadable file was overwritten")
	}
}

func TestEncryptedFile_ConcurrentSetsLoseNothing(t *testing.T) {
	f := newFile(t, "p")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := f.Set("k"+string(rune('a'+i)), New("v"+string(rune('a'+i)))); err != nil {
				t.Errorf("set %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		if g, err := f.Get("k" + string(rune('a'+i))); err != nil || g.Reveal() != "v"+string(rune('a'+i)) {
			t.Fatalf("lost update %d: %v", i, err)
		}
	}
}

func TestEncryptedFile_PassphraseProblems(t *testing.T) {
	for name, src := range map[string]func() (string, error){
		"nil":   nil,
		"error": func() (string, error) { return "", errors.New("no passphrase here") },
		"empty": func() (string, error) { return "", nil },
	} {
		f := &EncryptedFile{Path: filepath.Join(t.TempDir(), "s"), Passphrase: src}
		if err := f.Set("a", New("v")); err == nil {
			t.Errorf("%s: Set must fail", name)
		}
		if _, err := f.Get("a"); err == nil {
			t.Errorf("%s: Get must fail", name)
		}
		if _, err := os.Stat(f.Path); err == nil {
			t.Errorf("%s: a file was created without a usable passphrase", name)
		}
	}
	if err := (&EncryptedFile{Path: filepath.Join(t.TempDir(), "never")}).Delete("x"); err != nil {
		t.Fatalf("deleting from a store that does not exist must be a no-op: %v", err)
	}
}

func TestPassphraseFromEnvFile(t *testing.T) {
	t.Setenv("REPO_KEEPER_PASSPHRASE_FILE", "")
	if _, err := PassphraseFromEnvFile(); err == nil {
		t.Fatal("unset env must fail")
	}
	p := filepath.Join(t.TempDir(), "pass")
	_ = os.WriteFile(p, []byte("s3cret passphrase\n"), 0o600)
	t.Setenv("REPO_KEEPER_PASSPHRASE_FILE", p)
	if got, err := PassphraseFromEnvFile(); err != nil || got != "s3cret passphrase" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(p, 0o644) //nolint:gosec // deliberately loose
		if _, err := PassphraseFromEnvFile(); err == nil {
			t.Fatal("a world-readable passphrase file must be refused")
		}
	}
}

func TestDefaultIterations_MeetGuidance(t *testing.T) {
	f := &EncryptedFile{Path: filepath.Join(t.TempDir(), "s"), Passphrase: func() (string, error) { return "p", nil }}
	if err := f.Set("a", New("v")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.Path)
	var e envelope
	_ = json.Unmarshal(raw, &e)
	if e.Iter < 600_000 || e.KDF != "pbkdf2-sha256" {
		t.Fatalf("weak defaults: %+v", e)
	}
}

func FuzzDecrypt_ArbitraryFiles_NeverPanic(f *testing.F) {
	seedFile := &EncryptedFile{Path: filepath.Join(f.TempDir(), "s"), Passphrase: func() (string, error) { return "p", nil }, Iterations: minIter}
	_ = seedFile.Set("a", New("v"))
	good, _ := os.ReadFile(seedFile.Path)
	for _, s := range [][]byte{good, []byte("{}"), []byte(`{"v":1,"kdf":"pbkdf2-sha256","iter":100000,"salt":"","nonce":"","ct":""}`), {0, 255}} {
		f.Add(s)
	}
	cheap := func(string, []byte, int) ([]byte, error) { return make([]byte, 32), nil } // fuzzing must not spend 100k PBKDF2 rounds per input
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = decrypt(data, "p", cheap) })
}
