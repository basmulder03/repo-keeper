// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestScrub_KnownShapes_AreMasked(t *testing.T) {
	cases := map[string]string{
		"github classic": "token ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		"github finegr":  "github_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"gitlab":         "glpat-abcdefghij0123456789",
		"bearer header":  "Authorization: Bearer abcdefghijklmnop.qrstuvwxyz",
		"url userinfo":   "https://user:s3cretpass@example.com/repo.git",
		"basic":          "Basic dXNlcjpwYXNzd29yZDEyMzQ1",
	}
	var r Redactor
	for name, in := range cases {
		out := r.Scrub(in)
		if !strings.Contains(out, mask) {
			t.Errorf("%s: not masked: %q", name, out)
		}
	}
}

func TestScrub_RegisteredSecret_IsMasked(t *testing.T) {
	var r Redactor
	r.Add("hunter2-hunter2")
	r.Add("x") // too short, ignored
	if got := r.Scrub("pw=hunter2-hunter2 x"); got != "pw=[REDACTED] x" {
		t.Fatalf("got %q", got)
	}
}

func TestScrub_PlainText_Unchanged(t *testing.T) {
	var r Redactor
	in := "synced 12 repos in 3.2s"
	if got := r.Scrub(in); got != in {
		t.Fatalf("got %q", got)
	}
}

func TestNew_LogsAreRedacted_TextAndJSON(t *testing.T) {
	const canary = "CANARY-secret-value-123"
	for _, jsonFmt := range []bool{false, true} {
		var buf bytes.Buffer
		var r Redactor
		r.Add(canary)
		log := New(&buf, slog.LevelDebug, jsonFmt, &r)
		log.Info("auth "+canary, "url", "https://u:"+canary+"@h/x", "k", canary)
		if strings.Contains(buf.String(), canary) {
			t.Fatalf("json=%v leaked: %s", jsonFmt, buf.String())
		}
	}
}

func TestWriter_ReportsOriginalLength(t *testing.T) {
	var buf bytes.Buffer
	var r Redactor
	r.Add("longsecret")
	in := []byte("a longsecret b")
	n, err := r.Writer(&buf).Write(in)
	if err != nil || n != len(in) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
