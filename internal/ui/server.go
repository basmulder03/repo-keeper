// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
)

//go:embed templates/*.html static/*
var assets embed.FS

const (
	cookieName    = "rk_session"
	maxFormBytes  = 512 << 10
	maxConfigSize = 256 << 10
)

// Server is the UI HTTP server.
type Server struct {
	Backend Backend
	Log     *slog.Logger
	Clock   clock.Clock
	Version string

	auth    *auth
	control string
	addr    string
	pages   map[string]*template.Template
	hosts   map[string]bool
}

// RuntimeFile is what `repo-keeper ui` and the tray read to find the daemon.
type RuntimeFile struct {
	Addr    string    `json:"addr"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Control string    `json:"control"`
	Version string    `json:"version"`
}

// Listen binds a loopback port, preferring preferred and falling back to any free one. Never non-loopback.
func Listen(preferred int) (net.Listener, error) {
	var lc net.ListenConfig
	if preferred > 0 {
		l, err := lc.Listen(context.Background(), "tcp4", fmt.Sprintf("127.0.0.1:%d", preferred))
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) && !strings.Contains(err.Error(), "address already in use") && !strings.Contains(err.Error(), "Only one usage") {
			return nil, err
		}
	}
	return lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
}

// Serve runs the UI on l until ctx is cancelled. It writes runtimePath (mode 0600) so the CLI can find it.
func (s *Server) Serve(ctx context.Context, l net.Listener, runtimePath string) error {
	if s.Clock == nil {
		s.Clock = clock.Real{}
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	s.auth = newAuth(s.Clock)
	s.control = token()
	s.addr = l.Addr().String()
	_, port, _ := net.SplitHostPort(s.addr)
	s.hosts = map[string]bool{s.addr: true, "localhost:" + port: true}
	if err := s.parseTemplates(); err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if runtimePath != "" {
		if err := s.writeRuntime(runtimePath); err != nil {
			return err
		}
		defer func() { _ = os.Remove(runtimePath) }()
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	s.Log.Info("web UI listening", "addr", s.addr)
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) writeRuntime(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(RuntimeFile{Addr: s.addr, PID: os.Getpid(), Started: s.Clock.Now(), Control: s.control, Version: s.Version})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) parseTemplates() error {
	sub, err := fs.Sub(assets, "templates")
	if err != nil {
		return err
	}
	pages := []string{"dashboard", "repo", "accounts", "cleanup", "audit", "config", "debug", "error"}
	s.pages = map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New("layout.html").Funcs(funcs()).ParseFS(sub, "layout.html", "fragments.html", p+".html")
		if err != nil {
			return fmt.Errorf("ui: template %s: %w", p, err)
		}
		s.pages[p] = t
	}
	return nil
}

// Handler exposes the routes for tests.
func (s *Server) Handler() http.Handler { return s.routes() }

// Prepare initialises state without listening (tests drive Handler directly).
func (s *Server) Prepare(addr string) error {
	if s.Clock == nil {
		s.Clock = clock.Real{}
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	s.auth = newAuth(s.Clock)
	s.control = token()
	s.addr = addr
	_, port, _ := net.SplitHostPort(addr)
	s.hosts = map[string]bool{addr: true, "localhost:" + port: true}
	return s.parseTemplates()
}

// Control returns the control token (tests).
func (s *Server) Control() string { return s.control }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /api/login-url", s.handleLoginURL)

	mux.HandleFunc("GET /{$}", s.authed(s.handleDashboard))
	mux.HandleFunc("GET /fragments/dashboard", s.authed(s.handleDashboardFragment))
	mux.HandleFunc("GET /repos/{id}", s.authed(s.handleRepo))
	mux.HandleFunc("POST /repos/{id}/sync", s.authed(s.handleSync))
	mux.HandleFunc("POST /repos/{id}/cleanup", s.authed(s.handleCleanupNow))
	mux.HandleFunc("POST /repos/{id}/restore", s.authed(s.handleRestore))
	mux.HandleFunc("GET /accounts", s.authed(s.handleAccounts))
	mux.HandleFunc("POST /accounts/{name}/discover", s.authed(s.handleDiscover))
	mux.HandleFunc("GET /cleanup", s.authed(s.handleCleanup))
	mux.HandleFunc("GET /audit", s.authed(s.handleAudit))
	mux.HandleFunc("GET /config", s.authed(s.handleConfig))
	mux.HandleFunc("POST /config", s.authed(s.handleConfigSave))
	mux.HandleFunc("GET /debug", s.authed(s.handleDebug))
	mux.HandleFunc("GET /debug/bundle.json", s.authed(s.handleBundle))
	mux.HandleFunc("POST /logout", s.authed(s.handleLogout))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.fail(w, r, http.StatusNotFound, "Not found") })
	return s.guard(mux)
}

// guard applies, to every request: DNS-rebinding defence, security headers, size limits.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		if !s.hosts[strings.ToLower(r.Host)] { // blocks DNS rebinding: a rebound name carries a foreign Host
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin requires Origin (when sent) to match our host and rejects explicit cross-site fetches.
func (s *Server) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Scheme != "http" || !s.hosts[strings.ToLower(u.Host)] {
			return false
		}
	}
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return false
	}
	return true
}

type sessionInfo struct{ csrf, sid string }

// authed requires a valid session; POSTs additionally require the CSRF token.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, sessionInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			s.fail(w, r, http.StatusUnauthorized, "Not signed in. Run `repo-keeper ui` to open the interface.")
			return
		}
		csrf, ok := s.auth.lookup(c.Value)
		if !ok {
			s.fail(w, r, http.StatusUnauthorized, "Session expired. Run `repo-keeper ui` again.")
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil || !equal(r.PostFormValue("csrf"), csrf) {
				s.fail(w, r, http.StatusForbidden, "Invalid or missing form token. Reload the page and retry.")
				return
			}
		}
		h(w, r, sessionInfo{csrf: csrf, sid: c.Value})
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	sid, _, ok := s.auth.redeem(r.URL.Query().Get("code"))
	if !ok {
		s.fail(w, r, http.StatusUnauthorized, "This sign-in link is invalid or expired. Run `repo-keeper ui` for a new one.")
		return
	}
	// #nosec G124 -- Secure cannot be set on http://127.0.0.1; the cookie is HttpOnly + SameSite=Strict and host-bound
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see #nosec above
		Name: cookieName, Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther) // the code never stays in the address bar or history
}

// handleLoginURL mints a one-time login URL for the CLI; the control token in the 0600 runtime file is the credential.
func (s *Server) handleLoginURL(w http.ResponseWriter, r *http.Request) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) || !equal(strings.TrimPrefix(h, prefix), s.control) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": "http://" + s.addr + "/login?code=" + s.auth.mintCode()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, si sessionInfo) {
	s.auth.end(si.sid)
	// #nosec G124 -- clearing cookie; same loopback rationale
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // see #nosec above
	s.fail(w, r, http.StatusOK, "Signed out. Run `repo-keeper ui` to sign in again.")
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.WriteHeader(code)
	s.renderTo(w, "error", page{Title: http.StatusText(code), Message: msg, Anon: true})
}
