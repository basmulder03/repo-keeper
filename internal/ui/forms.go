// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/basmulder03/repo-keeper/internal/secrets"
)

func (s *Server) manageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings", s.authed(s.handleSettings))
	mux.HandleFunc("POST /settings", s.authed(s.handleSettingsSave))
	mux.HandleFunc("POST /settings/repos", s.authed(s.handleRepoAdd))
	mux.HandleFunc("POST /settings/repos/remove", s.authed(s.handleRepoRemove))

	mux.HandleFunc("GET /accounts/new", s.authed(s.handleAccountNew))
	mux.HandleFunc("POST /accounts", s.authed(s.handleAccountCreate))
	mux.HandleFunc("GET /accounts/{name}/edit", s.authed(s.handleAccountEdit))
	mux.HandleFunc("POST /accounts/{name}", s.authed(s.handleAccountUpdate))
	mux.HandleFunc("POST /accounts/{name}/remove", s.authed(s.handleAccountRemove))

	mux.HandleFunc("GET /devices/{id}", s.authed(s.handleDevice))
	mux.HandleFunc("POST /devices/{id}/cancel", s.authed(s.handleDeviceCancel))
}

// problems extracts user-fixable problems from an error; ok is false for genuine failures.
func problems(err error) ([]string, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Problems, true
	}
	return nil, false
}

// ---- settings

type settingsPage struct {
	SettingsView
	Problems     []string
	RepoProblems []string
	RepoForm     RepoForm
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, si sessionInfo, v SettingsView, code int, probs, repoProbs []string, rf RepoForm) {
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	s.render(w, r, si, "settings", "Settings", "settings", settingsPage{SettingsView: v, Problems: probs, RepoProblems: repoProbs, RepoForm: rf})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	v, err := s.Backend.Settings(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.renderSettings(w, r, si, v, http.StatusOK, nil, nil, RepoForm{})
}

func settingsFormFrom(r *http.Request) SettingsForm {
	v := r.PostFormValue
	on := func(k string) bool { return v(k) == "on" }
	return SettingsForm{
		Root: v("root"), Interval: v("interval"), Concurrency: v("concurrency"), PerHost: v("per_host"), QuietHours: v("quiet_hours"),
		AllBranches: on("all_branches"), Secrets: v("secrets"), SecretsFile: v("secrets_file"),
		CleanupMode: v("cleanup_mode"), MinAge: v("min_age"), Protected: v("protected"), AllowNeverPushed: on("allow_never_pushed"),
		UIEnabled: on("ui_enabled"), UIPort: v("ui_port"),
	}
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	f := settingsFormFrom(r)
	restart, err := s.Backend.SaveSettings(r.Context(), f)
	if err != nil {
		if p, ok := problems(err); ok {
			v, verr := s.Backend.Settings(r.Context())
			if verr != nil {
				s.internal(w, r, verr)
				return
			}
			v.Form = f // show what the user typed, not what is stored
			s.renderSettings(w, r, si, v, http.StatusUnprocessableEntity, p, nil, RepoForm{})
			return
		}
		s.internal(w, r, err)
		return
	}
	code := "settings-saved"
	if restart {
		code = "settings-saved-restart"
	}
	redirect(w, r, "/settings", code)
}

func (s *Server) handleRepoAdd(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	f := RepoForm{Path: r.PostFormValue("path"), Remote: r.PostFormValue("remote"), Interval: r.PostFormValue("interval"),
		CleanupMode: r.PostFormValue("cleanup_mode"), AllBranches: r.PostFormValue("all_branches")}
	if err := s.Backend.AddRepo(r.Context(), f); err != nil {
		if p, ok := problems(err); ok {
			v, verr := s.Backend.Settings(r.Context())
			if verr != nil {
				s.internal(w, r, verr)
				return
			}
			s.renderSettings(w, r, si, v, http.StatusUnprocessableEntity, nil, p, f)
			return
		}
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/settings", "repo-added")
}

func (s *Server) handleRepoRemove(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	if err := s.Backend.RemoveRepo(r.Context(), r.PostFormValue("path")); err != nil {
		if p, ok := problems(err); ok {
			v, _ := s.Backend.Settings(r.Context())
			s.renderSettings(w, r, si, v, http.StatusUnprocessableEntity, nil, p, RepoForm{})
			return
		}
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/settings", "repo-removed")
}

// ---- accounts

type accountPage struct {
	Form     AccountForm
	Problems []string
	Edit     bool
	NeedRoot bool
	Provs    []string
}

func (s *Server) renderAccountForm(w http.ResponseWriter, r *http.Request, si sessionInfo, f AccountForm, edit bool, probs []string, code int) {
	set, err := s.Backend.Settings(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	title := "Add account"
	if edit {
		title = "Edit account " + f.Name
	}
	s.render(w, r, si, "account_form", title, "accounts", accountPage{Form: f, Problems: probs, Edit: edit, NeedRoot: set.Form.Root == "", Provs: set.Providers})
}

func (s *Server) handleAccountNew(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	f := AccountForm{New: true, Provider: "github", Auth: "token", SkipArchived: true}
	if set, err := s.Backend.Settings(r.Context()); err == nil {
		f.Root = set.Form.Root
	}
	s.renderAccountForm(w, r, si, f, false, nil, http.StatusOK)
}

func accountFormFrom(r *http.Request) AccountForm {
	v := r.PostFormValue
	on := func(k string) bool { return v(k) == "on" }
	return AccountForm{
		Name: strings.TrimSpace(v("name")), Provider: v("provider"), BaseURL: v("base_url"), Auth: v("auth"),
		Token: secrets.New(strings.TrimSpace(v("token"))), TokenFile: v("token_file"), TokenEnv: v("token_env"),
		ClientID: v("client_id"), WebURL: v("web_url"), Scope: v("scope"),
		Include: v("include"), Exclude: v("exclude"), CAFile: v("ca_file"), CloneProtocol: v("clone_protocol"),
		SkipArchived: on("skip_archived"), SkipForks: on("skip_forks"), PartialClone: on("partial_clone"),
		DiscoveryInterval: v("discovery_interval"), Interval: v("interval"), CleanupMode: v("cleanup_mode"), Root: v("root"),
	}
}

func (s *Server) saveAccount(w http.ResponseWriter, r *http.Request, si sessionInfo, f AccountForm) {
	edit := !f.New
	if f.Auth == "device" {
		view, err := s.Backend.StartDeviceLogin(r.Context(), f)
		if err != nil {
			if p, ok := problems(err); ok {
				s.renderAccountForm(w, r, si, f, edit, p, http.StatusUnprocessableEntity)
				return
			}
			s.internal(w, r, err)
			return
		}
		http.Redirect(w, r, "/devices/"+url.PathEscape(view.ID), http.StatusSeeOther)
		return
	}
	if err := s.Backend.SaveAccount(r.Context(), f); err != nil {
		if p, ok := problems(err); ok {
			f.Token = secrets.Token{} // never send a pasted secret back to the browser
			s.renderAccountForm(w, r, si, f, edit, p, http.StatusUnprocessableEntity)
			return
		}
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/accounts", "account-saved")
}

func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	f := accountFormFrom(r)
	f.New = true
	s.saveAccount(w, r, si, f)
}

func (s *Server) handleAccountEdit(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	f, ok, err := s.Backend.AccountForm(r.Context(), r.PathValue("name"))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Account not found")
		return
	}
	s.renderAccountForm(w, r, si, f, true, nil, http.StatusOK)
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	f := accountFormFrom(r)
	f.Name = r.PathValue("name") // the identity comes from the URL, never from a form field
	f.New = false
	s.saveAccount(w, r, si, f)
}

func (s *Server) handleAccountRemove(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	if err := s.Backend.RemoveAccount(r.Context(), r.PathValue("name"), r.PostFormValue("delete_token") == "on"); err != nil {
		if p, ok := problems(err); ok {
			s.fail(w, r, http.StatusConflict, strings.Join(p, " "))
			return
		}
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/accounts", "account-removed")
}

// ---- device login

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	v, ok := s.Backend.DeviceLogin(r.PathValue("id"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "This sign-in is unknown or has expired. Start again from Accounts.")
		return
	}
	s.render(w, r, si, "device", "Sign in", "accounts", v)
}

func (s *Server) handleDeviceCancel(w http.ResponseWriter, r *http.Request, _ sessionInfo) {
	s.Backend.CancelDeviceLogin(r.PathValue("id"))
	redirect(w, r, "/accounts", "device-cancelled")
}
