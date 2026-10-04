// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"errors"
	"testing"
)

func TestSource_Optional_MissingEntryIsNoCredential_ButRealErrorsStillFail(t *testing.T) {
	m := &Mem{}
	if _, err := (Source{Key: "k"}).Resolve(m); !errors.Is(err, ErrNotFound) {
		t.Fatalf("required source must fail on a missing entry: %v", err)
	}
	tok, err := (Source{Key: "k", Optional: true}).Resolve(m)
	if err != nil || !tok.IsZero() {
		t.Fatalf("tok zero=%v err=%v", tok.IsZero(), err)
	}
	if _, err := (Source{Env: "REPO_KEEPER_TEST_UNSET_VAR", Optional: true}).Resolve(m); err == nil {
		t.Fatal("an env source that is configured but empty is a mistake, not 'no credential'")
	}
	_ = m.Set("k", New("secret"))
	if tok, err := (Source{Key: "k", Optional: true}).Resolve(m); err != nil || tok.Reveal() != "secret" {
		t.Fatalf("a stored token must still be used: %v", err)
	}
}
