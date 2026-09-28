// Package control is dootd's local admin API: HTTP/JSON over a root-only
// Unix socket, used by `dootd ctl`. Until the dashboard exists (Phase 4)
// it is the way to deploy; afterwards it remains an SSH fallback.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/internal/auth"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/github"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/supervisor"
)

// DefaultSocket is where dootd serve listens.
const DefaultSocket = "/run/dootd/dootd.sock"

// Server serves the control API.
type Server struct {
	Sup    *supervisor.Supervisor
	Dep    *deployer.Deployer
	Edge   *edge.Manager // nil when the edge is disabled
	Auth   *auth.Auth
	Layout layout.Layout
	Log    *slog.Logger
}

// AppView is one app in `GET /v1/apps`.
type AppView struct {
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Port       int       `json:"port"`
	Domain     string    `json:"domain,omitempty"`
	State      string    `json:"state"`
	PID        int       `json:"pid"`
	Release    string    `json:"release"`
	Restarts   int       `json:"restarts"`
	OOMKills   int64     `json:"oom_kills"`
	LastExit   string    `json:"last_exit,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	HealthyAt  time.Time `json:"healthy_at"`
	Deployable bool      `json:"deployable"`
	Repo       string    `json:"repo,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Subdir     string    `json:"path,omitempty"`
	Pending    int64     `json:"pending_deployment,omitempty"`
	MemBytes   int64     `json:"mem_bytes"`
	Requests   int64     `json:"requests"`
	Status5xx  int64     `json:"requests_5xx"`
}

// Serve listens on path until ctx ends.
func (s *Server) Serve(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("control: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/apps", s.apps)
	mux.HandleFunc("POST /v1/apps/{app}/deploy", s.deploy)
	mux.HandleFunc("POST /v1/apps/{app}/rollback", s.rollback)
	mux.HandleFunc("POST /v1/apps/{app}/{action}", s.action)
	mux.HandleFunc("GET /v1/apps/{app}/releases", s.releases)
	mux.HandleFunc("GET /v1/apps/{app}/deployments", s.deployments)
	mux.HandleFunc("GET /v1/apps/{app}/logs", s.appLogs)
	mux.HandleFunc("GET /v1/deployments/{id}", s.deployment)
	mux.HandleFunc("GET /v1/deployments/{id}/log", s.deploymentLog)
	mux.HandleFunc("PUT /v1/settings/github-token", s.githubToken)
	mux.HandleFunc("PUT /v1/settings/cloudflare-token", s.cloudflareToken)
	mux.HandleFunc("PUT /v1/admin", s.setAdmin)
	mux.HandleFunc("GET /v1/edge", s.edgeStatus)
	mux.HandleFunc("POST /v1/edge/sync", s.edgeSync)
	mux.HandleFunc("POST /v1/edge/zones/{zone}/ssl-strict", s.edgeStrict)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) app(w http.ResponseWriter, r *http.Request) *supervisor.App {
	a := s.Sup.Get(r.PathValue("app"))
	if a == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown app %q", r.PathValue("app")))
	}
	return a
}

func (s *Server) apps(w http.ResponseWriter, _ *http.Request) {
	var out []AppView
	var stats map[string]edge.StatsSnapshot
	if s.Edge != nil {
		stats = s.Edge.Router.Stats()
	}
	for _, a := range s.Sup.Apps() {
		sp, st := a.Spec(), a.Status()
		v := AppView{
			Name: sp.Name, Type: string(sp.Type), Port: sp.Port, Domain: sp.Domain, State: string(st.State), PID: st.PID,
			Release: st.Release, Restarts: st.Restarts, OOMKills: st.OOMKills, LastExit: st.LastExit,
			LastError: st.LastError, HealthyAt: st.HealthyAt,
		}
		if cfg, ok := s.Dep.Config(sp.Name); ok {
			v.Deployable, v.Repo, v.Branch, v.Subdir = true, displayRepo(cfg.Repo), cfg.Branch, cfg.Subdir
			v.Pending = s.Dep.Pending(sp.Name)
		}
		if gs, err := a.Group().Stats(); err == nil {
			v.MemBytes = gs.MemoryCurrent
		}
		if rs, ok := stats[sp.Name]; ok {
			v.Requests, v.Status5xx = rs.Requests, rs.Status[5]
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func displayRepo(r github.Repo) string {
	if r.GitHub {
		return r.Owner + "/" + r.Name
	}
	return r.URL
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	id, err := s.Dep.Deploy(r.Context(), r.PathValue("app"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int64{"id": id})
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Release string `json:"release"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Release == "" {
		writeErr(w, http.StatusBadRequest, errors.New(`body must be {"release": "<release id>"}`))
		return
	}
	id, err := s.Dep.Rollback(r.Context(), r.PathValue("app"), body.Release)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int64{"id": id})
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	a := s.app(w, r)
	if a == nil {
		return
	}
	name := a.Spec().Name
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var err error
	switch act := r.PathValue("action"); act {
	case "start":
		err = s.Dep.Start(ctx, name)
	case "stop":
		err = s.Dep.StopApp(ctx, name)
	case "restart":
		err = s.Dep.Restart(ctx, name)
	default:
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown action %q (start, stop, restart, deploy, rollback)", act))
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(a.Status().State)})
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Dep.Releases(r.Context(), r.PathValue("app"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (s *Server) deployments(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	ds, err := s.Dep.Deployments(r.Context(), r.PathValue("app"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

func (s *Server) deploymentID(w http.ResponseWriter, r *http.Request) (deployer.Deployment, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad deployment id"))
		return deployer.Deployment{}, false
	}
	d, err := s.Dep.Deployment(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return d, false
	}
	return d, true
}

func (s *Server) deployment(w http.ResponseWriter, r *http.Request) {
	if d, ok := s.deploymentID(w, r); ok {
		writeJSON(w, http.StatusOK, d)
	}
}

// deploymentLog streams a build log. With follow=1 it tails the file until
// the deployment has finished.
func (s *Server) deploymentLog(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deploymentID(w, r)
	if !ok {
		return
	}
	follow := r.URL.Query().Get("follow") == "1"
	path := s.Layout.BuildLog(d.App, d.ID)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fl, _ := w.(http.Flusher)
	var off int64
	buf := make([]byte, 64<<10)
	for {
		done := true
		if follow {
			cur, err := s.Dep.Deployment(r.Context(), d.ID)
			done = err != nil || cur.Done()
		}
		// Read everything written so far (after `done` was sampled, so
		// the final lines are never missed).
		if f, err := os.Open(path); err == nil {
			f.Seek(off, io.SeekStart)
			for {
				n, err := f.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
					off += int64(n)
				}
				if err != nil {
					break
				}
			}
			f.Close()
		}
		if fl != nil {
			fl.Flush()
		}
		if done {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	a := s.app(w, r)
	if a == nil {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 100
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var ch <-chan logs.Line
	if r.URL.Query().Get("follow") == "1" {
		c, cancel := a.Log().Subscribe(256)
		ch = c
		defer cancel()
	}
	for _, l := range a.Log().Recent(n) {
		fmt.Fprintln(w, l.String())
	}
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	if ch == nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintln(w, l.String())
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func (s *Server) githubToken(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	token := strings.TrimSpace(string(b))
	if err := s.Dep.SetGitHubToken(r.Context(), token); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if token == "" {
		writeJSON(w, http.StatusOK, map[string]string{"result": "GitHub token removed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info, err := (&github.API{Token: token}).Validate(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"result": "saved (encrypted)", "warning": "could not validate it with the GitHub API: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": "saved (encrypted); token belongs to " + info.Login})
}

func (s *Server) needEdge(w http.ResponseWriter) bool {
	if s.Edge == nil {
		writeErr(w, http.StatusConflict, errors.New(`the edge is disabled (edge.listen = "off")`))
		return false
	}
	return true
}

func (s *Server) cloudflareToken(w http.ResponseWriter, r *http.Request) {
	if !s.needEdge(w) {
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	zones, err := s.Edge.SetToken(ctx, strings.TrimSpace(string(b)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("token not saved: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": "saved (encrypted)", "zones": zones})
}

func (s *Server) edgeStatus(w http.ResponseWriter, r *http.Request) {
	if s.needEdge(w) {
		writeJSON(w, http.StatusOK, s.Edge.Status(r.Context()))
	}
}

func (s *Server) edgeSync(w http.ResponseWriter, r *http.Request) {
	if !s.needEdge(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	st, err := s.Edge.Sync(ctx)
	resp := map[string]any{"status": st}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) edgeStrict(w http.ResponseWriter, r *http.Request) {
	if !s.needEdge(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := s.Edge.SetStrict(ctx, r.PathValue("zone")); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": "SSL/TLS mode set to Full (strict)"})
}

func (s *Server) setAdmin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Email == "" {
		cur, err := s.Auth.Admin(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("no admin yet: pass --email"))
			return
		}
		body.Email = cur
	}
	if err := s.Auth.SetAdmin(r.Context(), body.Email, body.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.Log.Info("dashboard admin credentials set over the control socket; all sessions revoked")
	writeJSON(w, http.StatusOK, map[string]string{"result": "admin " + strings.ToLower(strings.TrimSpace(body.Email)) + " saved; all dashboard sessions were signed out"})
}
