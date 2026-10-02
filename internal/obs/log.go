// SPDX-License-Identifier: Apache-2.0

// Package obs configures structured logging with last-line-of-defence secret redaction.
package obs

import (
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

const mask = "[REDACTED]"

// Known token shapes; backstop only, secrets.Token is the primary defence.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`(?i)(bearer|token|basic)\s+[A-Za-z0-9._~+/=\-]{16,}`),
	regexp.MustCompile(`://[^/\s:@"]+:[^/\s@"]+@`), // userinfo password in URLs
}

// Redactor scrubs registered secrets and known token shapes from text.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// Add registers an exact secret value to scrub (ignored if shorter than 6 bytes).
func (r *Redactor) Add(secret string) {
	if len(secret) < 6 {
		return
	}
	r.mu.Lock()
	r.secrets = append(r.secrets, secret)
	r.mu.Unlock()
}

// Scrub returns s with secrets masked.
func (r *Redactor) Scrub(s string) string {
	r.mu.RLock()
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, mask)
	}
	r.mu.RUnlock()
	for _, p := range patterns {
		s = p.ReplaceAllStringFunc(s, func(m string) string {
			if strings.HasPrefix(m, "://") {
				return "://" + mask + "@"
			}
			return mask
		})
	}
	return s
}

// Writer wraps w so every Write is scrubbed (slog emits one Write per record).
func (r *Redactor) Writer(w io.Writer) io.Writer { return redactWriter{r: r, w: w} }

type redactWriter struct {
	r *Redactor
	w io.Writer
}

func (rw redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(rw.w, rw.r.Scrub(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil // report the caller's length, not the scrubbed one
}

// New builds a logger writing to w, text or JSON, with redaction applied to all output.
func New(w io.Writer, level slog.Leveler, jsonFmt bool, r *Redactor) *slog.Logger {
	if r == nil {
		r = &Redactor{}
	}
	opts := &slog.HandlerOptions{Level: level}
	out := r.Writer(w)
	if jsonFmt {
		return slog.New(slog.NewJSONHandler(out, opts))
	}
	return slog.New(slog.NewTextHandler(out, opts))
}
