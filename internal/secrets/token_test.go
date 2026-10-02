// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zalando/go-keyring"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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

func TestMem_Store_RoundTrip(t *testing.T) {
	var s Mem
	if _, err := s.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	_ = s.Set("a", New("v"))
	if got, _ := s.Get("a"); got.Reveal() != "v" {
		t.Fatal("round trip failed")
	}
	_ = s.Delete("a")
	if _, err := s.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("not deleted")
	}
}

func TestKeyring_Mocked_RoundTripAndMissing(t *testing.T) {
	keyring.MockInit()
	var k Keyring
	if _, err := k.Get("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	if err := k.Set("x", New(canary)); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get("x"); err != nil || got.Reveal() != canary {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if err := k.Delete("x"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("x"); err != nil {
		t.Fatalf("deleting a missing secret must be fine: %v", err)
	}
}

func TestSource_Resolve_EnvFileStore(t *testing.T) {
	var s Mem
	_ = s.Set("acct", New("from-store"))
	t.Setenv("RK_TEST_TOKEN", " from-env \n")
	dir := t.TempDir()
	good := filepath.Join(dir, "tok")
	_ = os.WriteFile(good, []byte("from-file\n"), 0o600)
	loose := filepath.Join(dir, "loose")
	_ = os.WriteFile(loose, []byte("x"), 0o644) //nolint:gosec // deliberately loose
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("  \n"), 0o600)

	for _, tc := range []struct {
		name string
		src  Source
		want string
		fail bool
	}{
		{"store", Source{Key: "acct"}, "from-store", false},
		{"env", Source{Env: "RK_TEST_TOKEN"}, "from-env", false},
		{"file", Source{File: good}, "from-file", false},
		{"missing store key", Source{Key: "nope"}, "", true},
		{"unset env", Source{Env: "RK_TEST_UNSET"}, "", true},
		{"missing file", Source{File: filepath.Join(dir, "nope")}, "", true},
		{"empty file", Source{File: empty}, "", true},
		{"loose perms", Source{File: loose}, "", runtime.GOOS != "windows"},
	} {
		got, err := tc.src.Resolve(&s)
		if (err != nil) != tc.fail || (!tc.fail && got.Reveal() != tc.want) {
			t.Errorf("%s: got=%q err=%v", tc.name, got.Reveal(), err)
		}
	}
}
