// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
)

const (
	codeTTL    = 60 * time.Second
	sessionTTL = 12 * time.Hour
	maxCodes   = 16
	maxSession = 64
)

// auth issues one-time login codes and sessions.
type auth struct {
	clk      clock.Clock
	mu       sync.Mutex
	codes    map[string]time.Time
	sessions map[string]session
}

type session struct {
	csrf    string
	expires time.Time
}

func newAuth(clk clock.Clock) *auth {
	return &auth{clk: clk, codes: map[string]time.Time{}, sessions: map[string]session{}}
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("ui: no system randomness: " + err.Error()) // continuing with predictable tokens would be worse
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// mintCode returns a single-use login code valid for 60 seconds.
func (a *auth) mintCode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clk.Now()
	for c, exp := range a.codes { // purge expired; also bounds memory if something spams the control API
		if now.After(exp) {
			delete(a.codes, c)
		}
	}
	if len(a.codes) >= maxCodes {
		for c := range a.codes {
			delete(a.codes, c)
			break
		}
	}
	c := token()
	a.codes[c] = now.Add(codeTTL)
	return c
}

// redeem consumes a code and opens a session; ok is false for unknown, used or expired codes.
func (a *auth) redeem(code string) (sid, csrf string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, found := a.codes[code]
	if !found {
		return "", "", false
	}
	delete(a.codes, code)
	now := a.clk.Now()
	if now.After(exp) {
		return "", "", false
	}
	for s, v := range a.sessions {
		if now.After(v.expires) {
			delete(a.sessions, s)
		}
	}
	if len(a.sessions) >= maxSession {
		for s := range a.sessions {
			delete(a.sessions, s)
			break
		}
	}
	sid, csrf = token(), token()
	a.sessions[sid] = session{csrf: csrf, expires: now.Add(sessionTTL)}
	return sid, csrf, true
}

// lookup returns the CSRF token of a live session.
func (a *auth) lookup(sid string) (csrf string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, found := a.sessions[sid]
	if !found || a.clk.Now().After(s.expires) {
		delete(a.sessions, sid)
		return "", false
	}
	return s.csrf, true
}

func (a *auth) end(sid string) {
	a.mu.Lock()
	delete(a.sessions, sid)
	a.mu.Unlock()
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
