// SPDX-License-Identifier: Apache-2.0

// Package secrets holds credential types that cannot leak through formatting, logging or JSON.
package secrets

import (
	"fmt"
	"log/slog"
)

const redacted = "[REDACTED]"

// Token is a secret string; every formatting path prints "[REDACTED]".
type Token struct{ v string }

// New wraps s as a Token.
func New(s string) Token { return Token{v: s} }

// Reveal returns the raw secret; call only at the point of use (auth header, askpass).
func (t Token) Reveal() string { return t.v }

// IsZero reports whether the token is empty.
func (t Token) IsZero() bool { return t.v == "" }

// String implements fmt.Stringer.
func (Token) String() string { return redacted }

// GoString implements fmt.GoStringer (%#v).
func (Token) GoString() string { return redacted }

// Format implements fmt.Formatter so no verb (%v %+v %x %q ...) can expose the value.
func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue implements slog.LogValuer.
func (Token) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler (covers JSON/TOML keys and values).
func (Token) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MarshalJSON implements json.Marshaler.
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
