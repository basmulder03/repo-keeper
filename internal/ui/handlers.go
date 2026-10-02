// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/audit"
	"github.com/basmulder03/repo-keeper/internal/cleanup"
	"github.com/basmulder03/repo-keeper/internal/gitx"
	"github.com/basmulder03/repo-keeper/internal/store"
)

// page is the common template input.
type page struct {
	Title   string
	Active  string
	CSRF    string
	Msg     string
	Err     string
	Message string
	Anon    bool
	Info    Info
	D       any
}

var messages = map[string]string{
	"sync-queued":   "Sync queued; it starts within a few seconds.",
	"discover":      "Discovery queued for that account.",
	"config-saved":  "Configuration saved. The daemon applies it within seconds.",
	"restored":      "Branch restored.",
	"cleanup-done":  "Cleanup finished.",
	"cleanup-empty": "Cleanup finished: nothing was safe to delete.",
}

func funcs() template.FuncMap {
	return template.FuncMap{
		"ago":    ago,
		"until":  until,
		"stamp":  stamp,
		"join":   strings.Join,
		"short":  short,
		"status": statusOf,
		"dur":    func(d time.Duration) string { return d.Round(time.Millisecond).String() },
		"lower":  strings.ToLower,
	}
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func until(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Until(t)
	switch {
	case d <= 0:
		return "due"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours())+1)
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

// statusOf maps a repo to a badge class and a human label; the label always carries the meaning, not only colour.
func statusOf(r store.Repo) [2]string {
	switch {
	case r.Missing:
		return [2]string{"muted", "no longer listed upstream"}
	case r.NeedsAttention:
		return [2]string{"err", "needs attention"}
	case r.LastStatus == "":
		return [2]string{"muted", "pending"}
	case r.LastStatus == "failed":
		return [2]string{"warn", "failed: " + r.LastReason}
	case r.LastFF == "diverged" || strings.HasPrefix(r.LastFF, "skipped") || r.LastFF == "local-ahead":
		return [2]string{"warn", "ok, default branch " + r.LastFF}
	}
	return [2]string{"ok", "up to date"}
}

func (s *Server) renderTo(w io.Writer, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		_, _ = io.WriteString(w, "template missing")
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.Log.Error("rendering page", "page", name, "err", err)
		_, _ = io.WriteString(w, "internal error")
		return
	}
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, si sessionInfo, name, title, active string, data any) {
	p := page{Title: title, Active: active, CSRF: si.csrf, Info: s.Backend.Info(), D: data}
	p.Msg = messages[r.URL.Query().Get("m")]
	if n := r.URL.Query().Get("n"); p.Msg != "" && n != "" {
		if v, err := strconv.Atoi(n); err == nil && v >= 0 && v < 100000 {
			p.Msg += " (" + strconv.Itoa(v) + " branch(es))"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderTo(w, name, p)
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("ui request failed", "path", r.URL.Path, "err", err)
	s.fail(w, r, http.StatusInternalServerError, "Something went wrong; see the daemon log for details.")
}

func repoID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

var safeNextRe = regexp.MustCompile(`^(/|/repos/[0-9]+|/cleanup|/accounts)$`)

// safeNext only ever returns a known in-app path, never a caller-supplied URL.
func safeNext(r *http.Request, fallback string) string {
	if n := r.PostFormValue("next"); safeNextRe.MatchString(n) {
		return n
	}
	return fallback
}

func redirect(w http.ResponseWriter, r *http.Request, to, code string) {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	// #nosec G710 -- `to` is always an in-app path built by the handlers or validated by safeNext
	http.Redirect(w, r, to+sep+"m="+code, http.StatusSeeOther) //nolint:gosec // see #nosec above
}

// ---- dashboard

type repoRow struct {
	store.Repo
	Class, Label string
}

type dashData struct {
	Rows     []repoRow
	Accounts []store.Account
	Hosts    any
	Events   []store.Event
	Counts   struct{ Total, OK, Attention, Pending int }
}

func (s *Server) dashboard(ctx context.Context) (dashData, error) {
	var d dashData
	repos, err := s.Backend.Repos(ctx)
	if err != nil {
		return d, err
	}
	for _, r := range repos {
		st := statusOf(r)
		d.Rows = append(d.Rows, repoRow{Repo: r, Class: st[0], Label: st[1]})
		d.Counts.Total++
		switch st[0] {
		case "ok":
			d.Counts.OK++
		case "err":
			d.Counts.Attention++
		case "muted":
			d.Counts.Pending++
		}
	}
	if d.Accounts, err = s.Backend.Accounts(ctx); err != nil {
		return d, err
	}
	if d.Events, err = s.Backend.Events(ctx, 8); err != nil {
		return d, err
	}
	d.Hosts = s.Backend.Hosts()
	return d, nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	d, err := s.dashboard(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, si, "dashboard", "Dashboard", "dashboard", d)
}

func (s *Server) handleDashboardFragment(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	d, err := s.dashboard(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := s.pages["dashboard"].ExecuteTemplate(&buf, "live", page{CSRF: si.csrf, D: d}); err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// ---- repo detail

type repoData struct {
	Repo    store.Repo
	Status  [2]string
	Runs    []store.Run
	Report  *cleanup.Report
	Trash   any
	Audit   []audit.Entry
	Cleanup []cleanup.Item
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	id, ok := repoID(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Not found")
		return
	}
	ctx := r.Context()
	repo, err := s.Backend.Repo(ctx, id)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "Repository not found")
		return
	}
	d := repoData{Repo: repo, Status: statusOf(repo)}
	if d.Runs, err = s.Backend.Runs(ctx, id, 20); err != nil {
		s.internal(w, r, err)
		return
	}
	for _, run := range d.Runs {
		if run.Detail != "" {
			var rep cleanup.Report
			if json.Unmarshal([]byte(run.Detail), &rep) == nil {
				d.Report = &rep
				break
			}
		}
	}
	if tr, err := s.Backend.Trash(ctx, id); err == nil {
		d.Trash = tr
	}
	if all, err := s.Backend.Audit(ctx, 1000); err == nil {
		for _, e := range all {
			if e.Repo == repo.Path {
				d.Audit = append(d.Audit, e)
			}
		}
	}
	s.render(w, r, si, "repo", repo.Path, "dashboard", d)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request, _ sessionInfo) {
	id, ok := repoID(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Not found")
		return
	}
	if err := s.Backend.SyncNow(r.Context(), id); err != nil {
		s.internal(w, r, err)
		return
	}
	redirect(w, r, safeNext(r, "/repos/"+strconv.FormatInt(id, 10)), "sync-queued")
}

func (s *Server) handleCleanupNow(w http.ResponseWriter, r *http.Request, _ sessionInfo) {
	id, ok := repoID(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Not found")
		return
	}
	rep, err := s.Backend.CleanupNow(r.Context(), id)
	if err != nil {
		if errors.Is(err, gitx.ErrLocked) {
			s.fail(w, r, http.StatusConflict, "This repository is being synced right now. Try again in a moment.")
			return
		}
		s.internal(w, r, err)
		return
	}
	deleted := 0
	for _, it := range rep.Items {
		if it.Outcome == cleanup.Deleted {
			deleted++
		}
	}
	to := "/repos/" + strconv.FormatInt(id, 10) + "?n=" + strconv.Itoa(deleted)
	code := "cleanup-done"
	if deleted == 0 {
		code = "cleanup-empty"
	}
	redirect(w, r, to, code)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request, _ sessionInfo) {
	id, ok := repoID(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Not found")
		return
	}
	branch := r.PostFormValue("branch")
	if branch == "" || len(branch) > 250 {
		s.fail(w, r, http.StatusBadRequest, "A branch name is required.")
		return
	}
	if err := s.Backend.Restore(r.Context(), id, branch); err != nil {
		s.fail(w, r, http.StatusConflict, "Could not restore: "+err.Error())
		return
	}
	redirect(w, r, "/repos/"+strconv.FormatInt(id, 10), "restored")
}

// ---- accounts, cleanup, audit

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	as, err := s.Backend.Accounts(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, si, "accounts", "Accounts", "accounts", map[string]any{"Accounts": as, "Hosts": s.Backend.Hosts()})
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request, _ sessionInfo) {
	if err := s.Backend.DiscoverNow(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, http.StatusNotFound, "Unknown account")
		return
	}
	redirect(w, r, "/accounts", "discover")
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	cv, err := s.Backend.Cleanups(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	var cand, total int
	for _, c := range cv {
		for _, it := range c.Report.Items {
			if it.Outcome == cleanup.WouldDelete || it.Outcome == cleanup.Deleted {
				cand++
			}
			total++
		}
	}
	s.render(w, r, si, "cleanup", "Cleanup", "cleanup", map[string]any{"Views": cv, "Candidates": cand, "Total": total})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	n := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 2000 {
		n = v
	}
	entries, err := s.Backend.Audit(r.Context(), n)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q != "" {
		var f []audit.Entry
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Repo+" "+e.Branch+" "+string(e.Action)+" "+e.Reason), q) {
				f = append(f, e)
			}
		}
		entries = f
	}
	s.render(w, r, si, "audit", "Audit journal", "audit", map[string]any{"Entries": entries, "Q": q, "N": n, "Sizes": []int{100, 200, 500, 1000}})
}

// ---- config

type configData struct {
	Text    string
	Version string
	Errors  []string
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	text, ver, err := s.Backend.Config()
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, si, "config", "Configuration", "config", configData{Text: text, Version: ver})
}

func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	text := strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")
	if len(text) > maxConfigSize {
		s.fail(w, r, http.StatusRequestEntityTooLarge, "Configuration is too large.")
		return
	}
	if err := s.Backend.SaveConfig(r.Context(), text, r.PostFormValue("version")); err != nil {
		cd := configData{Text: text, Version: r.PostFormValue("version"), Errors: strings.Split(strings.TrimSpace(err.Error()), "\n")}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, si, "config", "Configuration", "config", cd)
		return
	}
	redirect(w, r, "/config", "config-saved")
}

// ---- debug

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	ctx := r.Context()
	ev, err := s.Backend.Events(ctx, 100)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	cmds := s.Backend.Commands()
	for i, j := 0, len(cmds)-1; i < j; i, j = i+1, j-1 { // newest first
		cmds[i], cmds[j] = cmds[j], cmds[i]
	}
	s.render(w, r, si, "debug", "Debug", "debug", map[string]any{"Events": ev, "Commands": cmds, "Hosts": s.Backend.Hosts()})
}

func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request, _ sessionInfo) {
	b, err := s.Backend.Bundle(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="repo-keeper-diagnostics.json"`)
	_, _ = w.Write(b)
}
