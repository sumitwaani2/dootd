// Package web is the dashboard: server-rendered HTML with a small script
// for live logs and status refresh. It is served on the dashboard domain
// through the edge (docs/architecture.md §14).
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/internal/apps"
	"github.com/sumitwaani2/dootd/internal/auth"
	"github.com/sumitwaani2/dootd/internal/backup"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/metrics"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
	"github.com/sumitwaani2/dootd/internal/toolchain"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	sessionCookie = "__Host-dootd"
	flashCookie   = "__Host-dootd-flash"
)

// Server is the dashboard.
type Server struct {
	Auth    *auth.Auth
	Apps    *apps.Service
	Dep     *deployer.Deployer
	Sup     *supervisor.Supervisor
	Edge    *edge.Manager // nil when the edge is disabled
	Zig     *toolchain.Zig
	Store   *store.Store
	Backups *backup.Service
	// MasterKeyPath is included in the recovery kit.
	MasterKeyPath string
	Metrics       *metrics.Collector
	Thresholds    Thresholds
	Layout        layout.Layout
	Host          string // dashboard hostname
	Version       string
	Log           *slog.Logger

	pages map[string]*template.Template
}

type ctxKey int

const sessionKey ctxKey = 0

func session(r *http.Request) *auth.Session {
	s, _ := r.Context().Value(sessionKey).(*auth.Session)
	return s
}

// Handler builds the dashboard handler.
func (s *Server) Handler() (http.Handler, error) {
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)

	authed := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireSession(h)) }
	authed("POST /logout", s.logout)
	authed("GET /{$}", s.home)
	authed("GET /apps/new", s.newAppPage)
	authed("POST /apps", s.createApp)
	authed("GET /apps/{app}", s.appPage)
	authed("POST /apps/{app}/deploy", s.deployApp)
	authed("POST /apps/{app}/rollback", s.rollbackApp)
	authed("POST /apps/{app}/start", s.appAction)
	authed("POST /apps/{app}/stop", s.appAction)
	authed("POST /apps/{app}/restart", s.appAction)
	authed("POST /apps/{app}/settings", s.updateApp)
	authed("POST /apps/{app}/env", s.setEnv)
	authed("POST /apps/{app}/env/delete", s.deleteEnv)
	authed("POST /apps/{app}/delete", s.deleteApp)
	authed("POST /apps/{app}/backup", s.backupNow)
	authed("POST /apps/{app}/restore", s.restoreBackup)
	authed("POST /settings/s3", s.setS3)
	authed("POST /settings/recovery-kit", s.recoveryKit)
	authed("GET /apps/{app}/logs", s.logsPage)
	authed("GET /apps/{app}/logs/stream", s.logsStream)
	authed("GET /deployments/{id}", s.deploymentPage)
	authed("GET /deployments/{id}/stream", s.deploymentStream)
	authed("GET /settings", s.settingsPage)
	authed("GET /metrics", s.metricsPage)
	authed("GET /charts", s.chartSVG)
	authed("POST /settings/github-token", s.setGitHubToken)
	authed("POST /settings/cloudflare-token", s.setCloudflareToken)
	authed("POST /settings/edge-sync", s.edgeSync)
	authed("POST /settings/ssl-strict", s.sslStrict)
	authed("POST /settings/toolchains/delete", s.deleteToolchain)
	authed("GET /account", s.accountPage)
	authed("POST /account/password", s.changePassword)
	authed("POST /account/revoke-others", s.revokeOthers)
	return s.secure(mux), nil
}

func staticHandler(fsys fs.FS) http.Handler {
	fh := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fh.ServeHTTP(w, r)
	})
}

// secure adds security headers and rejects cross-origin POSTs.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		next.ServeHTTP(w, r)
	})
}

// sameOrigin requires Origin (or, failing that, Referer) to be the dashboard.
func (s *Server) sameOrigin(r *http.Request) bool {
	want := "https://" + s.Host
	if o := r.Header.Get("Origin"); o != "" {
		return o == want
	}
	ref, err := url.Parse(r.Header.Get("Referer"))
	return err == nil && ref.Scheme == "https" && ref.Host == s.Host
}

// requireSession loads the session and checks the CSRF token on POSTs.
func (s *Server) requireSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		var sess *auth.Session
		if err == nil {
			sess, err = s.Auth.Session(r.Context(), c.Value)
		}
		if err != nil || sess == nil {
			if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/stream") {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
				return
			}
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && !sess.CheckCSRF(r.FormValue("csrf")) {
			http.Error(w, "invalid or missing CSRF token; reload the page and try again", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

func setSessionCookie(w http.ResponseWriter, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// flash stores a one-time message for the next page.
func flash(w http.ResponseWriter, ok bool, msg string) {
	if len(msg) > 1500 {
		msg = msg[:1500] + "…"
	}
	kind := "err:"
	if ok {
		kind = "ok:"
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(kind + msg)), Path: "/", MaxAge: 60,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func takeFlash(w http.ResponseWriter, r *http.Request) (msg string, ok bool) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return "", false
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return "", false
	}
	v := string(b)
	if m, found := strings.CutPrefix(v, "ok:"); found {
		return m, true
	}
	return strings.TrimPrefix(v, "err:"), false
}

// redirect finishes a POST with a flash message.
func redirect(w http.ResponseWriter, r *http.Request, to string, err error, okMsg string) {
	if err != nil {
		flash(w, false, err.Error())
	} else if okMsg != "" {
		flash(w, true, okMsg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// Page is the data every template receives.
type Page struct {
	Title    string
	Nav      string
	CSRF     string
	Admin    string
	Version  string
	Flash    string
	FlashOK  bool
	Warnings []string
	Data     any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title, nav string, data any) {
	p := Page{Title: title, Nav: nav, Version: s.Version, Data: data}
	if sess := session(r); sess != nil {
		p.CSRF = sess.CSRF
		p.Admin, _ = s.Auth.Admin(r.Context())
	}
	p.Flash, p.FlashOK = takeFlash(w, r)
	t := s.pages[name]
	if t == nil {
		http.Error(w, "unknown page "+name, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.Log.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"ago":      ago,
		"datetime": func(t time.Time) string { return fmtTime(t, time.DateTime) },
		"date":     func(t time.Time) string { return fmtTime(t, time.DateOnly) },
		"bytes":    humanBytes,
		"short": func(s string) string {
			if len(s) > 10 {
				return s[:10]
			}
			return s
		},
		"dur": func(a, b time.Time) string {
			if a.IsZero() || b.IsZero() {
				return "-"
			}
			return b.Sub(a).Round(100 * time.Millisecond).String()
		},
		"pct":   func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
		"int64": func(f float64) int64 { return int64(f) },
		"ratio": func(a, b int64) float64 {
			if b <= 0 {
				return 0
			}
			return float64(a) * 100 / float64(b)
		},
	}
	s.pages = map[string]*template.Template{}
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := strings.TrimSuffix(path.Base(e), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", e)
		if err != nil {
			return fmt.Errorf("web: template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

func fmtTime(t time.Time, layout string) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format(layout)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "in " + (-d).Round(time.Minute).String()
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// errorPage renders a simple error inside the layout.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, err error) {
	s.render(w, r, status, "error", "Error", "", map[string]any{"Status": status, "Message": err.Error()})
}

var errNotFound = errors.New("not found")
