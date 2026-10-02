// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/basmulder03/repo-keeper/internal/instance"
)

// ErrBadPassphrase means decryption failed: wrong passphrase or a modified file (AES-GCM cannot tell which).
var ErrBadPassphrase = errors.New("secrets: wrong passphrase or corrupted secrets file")

const (
	fileVersion  = 1
	kdfName      = "pbkdf2-sha256"
	defaultIter  = 600_000 // OWASP guidance for PBKDF2-HMAC-SHA256
	minIter      = 100_000 // a tampered file must not downgrade the work factor
	maxIter      = 10_000_000
	aad          = "repo-keeper/secrets/v1"
	maxFileBytes = 1 << 20
)

// EncryptedFile stores all secrets in one passphrase-encrypted file for hosts with no OS keychain.
// Every write uses a fresh salt and nonce; the plaintext never touches the disk.
type EncryptedFile struct {
	Path string
	// Passphrase supplies the passphrase (see PassphraseFromFile); it must not prompt interactively when used by the daemon.
	Passphrase func() (string, error)
	// Iterations overrides the PBKDF2 work factor for new files (tests); 0 = default.
	Iterations int

	mu sync.Mutex
}

type envelope struct {
	Version int    `json:"v"`
	KDF     string `json:"kdf"`
	Iter    int    `json:"iter"`
	Salt    string `json:"salt"`
	Nonce   string `json:"nonce"`
	Data    string `json:"ct"`
}

type payload struct {
	Secrets map[string]string `json:"secrets"`
}

// PassphraseFromEnvFile reads the passphrase from the file named by $REPO_KEEPER_PASSPHRASE_FILE (mode 600,
// like a token file): systemd LoadCredential=, agenix and sops-nix all provide exactly that.
func PassphraseFromEnvFile() (string, error) {
	p := os.Getenv("REPO_KEEPER_PASSPHRASE_FILE")
	if p == "" {
		return "", errors.New("secrets: set REPO_KEEPER_PASSPHRASE_FILE to a mode-600 file containing the secrets passphrase")
	}
	t, err := readTokenFile(p)
	if err != nil {
		return "", err
	}
	return t.Reveal(), nil
}

func (f *EncryptedFile) key(pass string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pass, salt, iter, 32)
}

// load decrypts the file; a missing file is an empty store.
func (f *EncryptedFile) load(pass string) (map[string]string, error) {
	raw, err := os.ReadFile(f.Path) //nolint:gosec // #nosec G304 -- configured secrets file
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return decrypt(raw, pass, f.key)
}

func decrypt(raw []byte, pass string, derive func(string, []byte, int) ([]byte, error)) (map[string]string, error) {
	if len(raw) > maxFileBytes {
		return nil, errors.New("secrets: file too large")
	}
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("secrets: not a repo-keeper secrets file: %w", err)
	}
	if e.Version != fileVersion || e.KDF != kdfName || e.Iter < minIter || e.Iter > maxIter {
		return nil, errors.New("secrets: unsupported or unsafe secrets file parameters")
	}
	salt, err1 := base64.StdEncoding.DecodeString(e.Salt)
	nonce, err2 := base64.StdEncoding.DecodeString(e.Nonce)
	ct, err3 := base64.StdEncoding.DecodeString(e.Data)
	if err1 != nil || err2 != nil || err3 != nil || len(salt) < 16 {
		return nil, errors.New("secrets: corrupted secrets file")
	}
	k, err := derive(pass, salt, e.Iter)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("secrets: corrupted secrets file")
	}
	plain, err := gcm.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		return nil, ErrBadPassphrase
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil || p.Secrets == nil {
		return nil, errors.New("secrets: corrupted secrets file")
	}
	return p.Secrets, nil
}

func (f *EncryptedFile) save(pass string, m map[string]string) error {
	iter := f.Iterations
	if iter == 0 {
		iter = defaultIter
	}
	salt, nonce := make([]byte, 16), make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	k, err := f.key(pass, salt, iter)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(payload{Secrets: m})
	if err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString
	out, err := json.Marshal(envelope{
		Version: fileVersion, KDF: kdfName, Iter: iter, Salt: enc(salt), Nonce: enc(nonce),
		Data: enc(gcm.Seal(nil, nonce, plain, []byte(aad))),
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".secrets-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

func (f *EncryptedFile) pass() (string, error) {
	if f.Passphrase == nil {
		return "", errors.New("secrets: no passphrase source configured")
	}
	p, err := f.Passphrase()
	if err != nil {
		return "", err
	}
	if p == "" {
		return "", errors.New("secrets: empty passphrase")
	}
	return p, nil
}

// modify serialises read-modify-write across processes (daemon and CLI) with an OS file lock.
func (f *EncryptedFile) modify(fn func(m map[string]string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pass, err := f.pass()
	if err != nil {
		return err
	}
	lock, err := instance.Acquire(f.Path + ".lock")
	if err != nil {
		return fmt.Errorf("secrets: file is being updated by another process: %w", err)
	}
	defer lock.Release()
	m, err := f.load(pass)
	if err != nil {
		return err // never overwrite a file we could not read
	}
	fn(m)
	return f.save(pass, m)
}

// Get implements Store.
func (f *EncryptedFile) Get(name string) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pass, err := f.pass()
	if err != nil {
		return Token{}, err
	}
	m, err := f.load(pass)
	if err != nil {
		return Token{}, err
	}
	if v, ok := m[name]; ok {
		return New(v), nil
	}
	return Token{}, ErrNotFound
}

// Set implements Store.
func (f *EncryptedFile) Set(name string, t Token) error {
	return f.modify(func(m map[string]string) { m[name] = t.Reveal() })
}

// Delete implements Store; deleting a missing secret is not an error (and never creates the file).
func (f *EncryptedFile) Delete(name string) error {
	if _, err := os.Stat(f.Path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return f.modify(func(m map[string]string) { delete(m, name) })
}
