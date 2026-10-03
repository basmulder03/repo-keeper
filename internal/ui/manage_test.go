// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/config"
)

// manageState lets tests script the Manage side of the fake backend and see exactly what the UI submitted.
type manageState struct {
	view        SettingsView
	settings    []SettingsForm
	accountsIn  []AccountForm
	repos       []RepoForm
	removedRepo []string
	removedAcct []string
	deleteToken bool
	restart     bool
	err         error
	devices     map[string]DeviceView
	cancelled   []string
	known       map[string]AccountForm
}

func newManageState() *manageState {
	return &manageState{
		view: SettingsView{
			Form: SettingsForm{Root: "/home/u/code", Interval: "30m", Concurrency: "4", PerHost: "2", AllBranches: true, Secrets: "keyring",
				CleanupMode: "dry-run", MinAge: "7d", Protected: "main\nrelease/*", UIEnabled: true, UIPort: "7878"},
			Repos:     []config.Repo{{Path: "/home/u/dotfiles"}, {Path: "/x/" + xss}},
			Providers: []string{"github", "gitlab"},
		},
		devices: map[string]DeviceView{},
		known:   map[string]AccountForm{"gh": {Name: "gh", Provider: "github", Auth: "keep", Include: "me/*", SkipArchived: true}},
	}
}

func (f *fake) Settings(context.Context) (SettingsView, error) { return f.m.view, nil }

func (f *fake) SaveSettings(_ context.Context, s SettingsForm) (bool, error) {
	f.rec("save-settings")
	f.m.settings = append(f.m.settings, s)
	return f.m.restart, f.m.err
}

func (f *fake) AddRepo(_ context.Context, r RepoForm) error {
	f.rec("add-repo")
	f.m.repos = append(f.m.repos, r)
	return f.m.err
}

func (f *fake) RemoveRepo(_ context.Context, p string) error {
	f.rec("remove-repo")
	f.m.removedRepo = append(f.m.removedRepo, p)
	return f.m.err
}

func (f *fake) AccountForm(_ context.Context, name string) (AccountForm, bool, error) {
	a, ok := f.m.known[name]
	return a, ok, nil
}

func (f *fake) SaveAccount(_ context.Context, a AccountForm) error {
	f.rec("save-account")
	f.m.accountsIn = append(f.m.accountsIn, a)
	return f.m.err
}

func (f *fake) RemoveAccount(_ context.Context, name string, del bool) error {
	f.rec("remove-account")
	f.m.removedAcct, f.m.deleteToken = append(f.m.removedAcct, name), del
	return f.m.err
}

func (f *fake) StartDeviceLogin(_ context.Context, a AccountForm) (DeviceView, error) {
	f.rec("start-device")
	f.m.accountsIn = append(f.m.accountsIn, a)
	if f.m.err != nil {
		return DeviceView{}, f.m.err
	}
	v := DeviceView{ID: "dev1", Account: a.Name, UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresAt: time.Now().Add(15 * time.Minute), State: "pending"}
	f.m.devices[v.ID] = v
	return v, nil
}

func (f *fake) DeviceLogin(id string) (DeviceView, bool) { v, ok := f.m.devices[id]; return v, ok }
func (f *fake) CancelDeviceLogin(id string) {
	f.rec("cancel-device")
	f.m.cancelled = append(f.m.cancelled, id)
}

var errBoom = errors.New("disk exploded")

func (e *env) post(path string, form url.Values, c *http.Cookie, csrf string) int {
	form.Set("csrf", csrf)
	return e.do(http.MethodPost, path, form, cookie(c)).Code
}

func has(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}
