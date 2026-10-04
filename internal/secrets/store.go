// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// ErrNotFound means no secret is stored under that name.
var ErrNotFound = errors.New("secrets: not found")

const keyringService = "repo-keeper"

// Store persists named secrets.
type Store interface {
	Get(name string) (Token, error)
	Set(name string, t Token) error
	Delete(name string) error
}

// Keyring uses the OS credential store (Secret Service, Keychain, Credential Manager).
type Keyring struct{}

// Get implements Store.
func (Keyring) Get(name string) (Token, error) {
	v, err := keyring.Get(keyringService, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, fmt.Errorf("secrets: keyring unavailable: %w", err)
	}
	return New(v), nil
}

// Set implements Store.
func (Keyring) Set(name string, t Token) error {
	if err := keyring.Set(keyringService, name, t.Reveal()); err != nil {
		return fmt.Errorf("secrets: keyring unavailable: %w", err)
	}
	return nil
}

// Delete implements Store; deleting a missing secret is not an error.
func (Keyring) Delete(name string) error {
	if err := keyring.Delete(keyringService, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("secrets: keyring unavailable: %w", err)
	}
	return nil
}

// Mem is an in-memory Store for tests.
type Mem struct {
	mu sync.Mutex
	m  map[string]Token
}

// Get implements Store.
func (s *Mem) Get(name string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.m[name]; ok {
		return t, nil
	}
	return Token{}, ErrNotFound
}

// Set implements Store.
func (s *Mem) Set(name string, t Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Token{}
	}
	s.m[name] = t
	return nil
}

// Delete implements Store.
func (s *Mem) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return nil
}

// Source says where an account's token comes from; at most one of Env/File, otherwise the Store.
type Source struct {
	Env  string // environment variable name
	File string // path to a file holding only the token (agenix/sops/systemd credentials)
	Key  string // name in the Store
	// Optional makes a missing Store entry a valid "no credential" (zero token) instead of an error.
	Optional bool
}

const maxTokenFile = 4096

// Resolve fetches the token from the configured source.
func (s Source) Resolve(store Store) (Token, error) {
	switch {
	case s.Env != "":
		v := strings.TrimSpace(os.Getenv(s.Env))
		if v == "" {
			return Token{}, fmt.Errorf("secrets: environment variable %s is empty or unset", s.Env)
		}
		return New(v), nil
	case s.File != "":
		return readTokenFile(s.File)
	default:
		t, err := store.Get(s.Key)
		if s.Optional && errors.Is(err, ErrNotFound) {
			return Token{}, nil // no credential is a valid setup here (SSH keys, public repositories)
		}
		return t, err
	}
}

// readTokenFile reads a token file, refusing group/world-accessible files on Unix (like ssh does for keys).
func readTokenFile(path string) (Token, error) {
	// #nosec G304 G703 -- user-configured secret file (token/passphrase), opened read-only
	f, err := os.Open(path) //nolint:gosec // see #nosec above
	if err != nil {
		return Token{}, fmt.Errorf("secrets: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return Token{}, err
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return Token{}, fmt.Errorf("secrets: %s is accessible by other users (mode %o); chmod 600 it", path, st.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return Token{}, err
	}
	if len(b) > maxTokenFile {
		return Token{}, fmt.Errorf("secrets: %s is too large for a token", path)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return Token{}, fmt.Errorf("secrets: %s is empty", path)
	}
	return New(v), nil
}
