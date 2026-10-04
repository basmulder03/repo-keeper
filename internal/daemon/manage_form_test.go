// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/basmulder03/repo-keeper/internal/ui"
)

func TestBuildAccount_GenericGit_URLsAndNoCredential(t *testing.T) {
	f := ui.AccountForm{
		Name: "internal", Provider: "git", Auth: "none", SkipArchived: true,
		URLs: "git@git.example.com:team/app.git\n https://git.example.com/team/tools.git \n\n",
	}
	a, problems := buildAccount(f, nil)
	if len(problems) != 0 {
		t.Fatalf("problems=%v", problems)
	}
	want := []string{"git@git.example.com:team/app.git", "https://git.example.com/team/tools.git"}
	if !reflect.DeepEqual(a.URLs, want) || a.TokenFile != "" || a.TokenEnv != "" {
		t.Fatalf("a=%+v", a)
	}
}

func TestBuildAccount_NoCredential_OnlyForGenericGit(t *testing.T) {
	_, problems := buildAccount(ui.AccountForm{Name: "gh", Provider: "github", Auth: "none"}, nil)
	if len(problems) != 1 || !strings.Contains(problems[0], "Only generic git") {
		t.Fatalf("problems=%v", problems)
	}
}
