// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const canary = "CANARY-ghp_0123456789abcdefghijklmnopqrstuvwxyz"

func TestToken_Formatting_NeverLeaks(t *testing.T) {
	tok := New(canary)
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T%v"}
	for _, v := range verbs {
		for _, arg := range []any{tok, &tok, []Token{tok}, map[string]Token{"k": tok}, struct{ T Token }{tok}} {
			if out := fmt.Sprintf(v, arg); strings.Contains(out, canary) {
				t.Fatalf("verb %s leaked: %s", v, out)
			}
		}
	}
}

func TestToken_JSON_NeverLeaks(t *testing.T) {
	b, err := json.Marshal(struct{ T Token }{New(canary)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(canary)) || !bytes.Contains(b, []byte(redacted)) {
		t.Fatalf("json = %s", b)
	}
}

func TestToken_Slog_NeverLeaks(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "tok", New(canary))
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "tok", New(canary))
	if strings.Contains(buf.String(), canary) {
		t.Fatalf("slog leaked: %s", buf.String())
	}
}

func TestToken_Reveal_ReturnsRaw(t *testing.T) {
	if got := New(canary).Reveal(); got != canary {
		t.Fatalf("Reveal() = %q", got)
	}
	if !(Token{}).IsZero() || New("a").IsZero() {
		t.Fatal("IsZero wrong")
	}
}

func TestToken_TextAndGoString_AreRedacted(t *testing.T) {
	tok := New(canary)
	b, err := tok.MarshalText()
	if err != nil || string(b) != redacted || tok.GoString() != redacted || tok.String() != redacted {
		t.Fatalf("text=%q err=%v", b, err)
	}
}
