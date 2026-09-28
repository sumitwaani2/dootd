package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/internal/apps"
	"github.com/sumitwaani2/dootd/internal/auth"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/github"
	"github.com/sumitwaani2/dootd/internal/hostinfo"
	"github.com/sumitwaani2/dootd/internal/supervisor"
	"github.com/sumitwaani2/dootd/internal/toolchain"
)

const settingGitHubLogin = "github_login"

// ---------------------------------------------------------------- auth

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, err := s.Auth.Session(r.Context(), c.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	data := map[string]any{"Next": safeNext(r.URL.Query().Get("next"))}
	if _, err := s.Auth.Admin(r.Context()); errors.Is(err, auth.ErrNoAdmin) {
		data["NoAdmin"] = err.Error()
	}
	s.render(w, r, http.StatusOK, "login", "Sign in", "", data)
}

// safeNext only allows local absolute paths as redirect targets.
func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.ContainsAny(n, "\\\r\n") {
		return "/"
	}
	return n
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	email, next := r.FormValue("email"), safeNext(r.FormValue("next"))
	tok, err := s.Auth.Login(r.Context(), email, r.FormValue("password"), edge.ClientIP(r), r.UserAgent())
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrRateLimited) {
			status = http.StatusTooManyRequests
		} else if !errors.Is(err, auth.ErrInvalidLogin) && !errors.Is(err, auth.ErrNoAdmin) {
			s.Log.Error("login", "err", err)
			err = errors.New("sign-in failed; see the dootd logs")
			status = http.StatusInternalServerError
		}
		s.Log.Warn("failed sign-in", "ip", edge.ClientIP(r), "reason", err)
		s.render(w, r, status, "login", "Sign in", "", map[string]any{"Error": err.Error(), "Email": email, "Next": next})
		return
	}
	s.Log.Info("signed in", "ip", edge.ClientIP(r))
	setSessionCookie(w, tok, int(auth.AbsoluteTimeout/time.Second))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Logout(r.Context(), session(r))
	setSessionCookie(w, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------------------------------------------------------------- home

// AppRow is an app in lists.
type AppRow struct {
	apps.App
	Status        supervisor.Status
	Pending       int64
	NeedsRestart  bool
	MemBytes      int64
	Requests      int64
	Requests5xx   int64
	LastDeployErr string
}

func (s *Server) appRows(ctx context.Context) ([]AppRow, error) {
	list, err := s.Apps.List(ctx)
	if err != nil {
		return nil, err
	}
	var stats map[string]edge.StatsSnapshot
	if s.Edge != nil {
		stats = s.Edge.Router.Stats()
	}
	var rows []AppRow
	for _, a := range list {
		row := AppRow{App: a}
		if sa := s.Sup.Get(a.Name); sa != nil {
			row.Status = sa.Status()
			if gs, err := sa.Group().Stats(); err == nil {
				row.MemBytes = gs.MemoryCurrent
			}
		}
		row.Pending = s.Dep.Pending(a.Name)
		row.NeedsRestart = s.Dep.NeedsRestart(a.Name)
		if st, ok := stats[a.Name]; ok {
			row.Requests, row.Requests5xx = st.Requests, st.Status[5]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) warnings(ctx context.Context, rows []AppRow) []string {
	var ws []string
	if _, ok, _ := s.Store.GetSetting(ctx, deployer.SettingGitHubToken); !ok {
		ws = append(ws, "No GitHub token is set, so only public repositories can be deployed. Add one in Settings.")
	}
	if s.Edge == nil {
		ws = append(ws, "The edge is disabled (edge.listen = \"off\"); apps are not reachable from the internet.")
	} else {
		st := s.Edge.Status(ctx)
		if !st.TokenSet {
			ws = append(ws, "No Cloudflare token is set, so DNS records and certificates cannot be managed. Add one in Settings.")
		}
		for _, z := range st.Zones {
			if z.Warning != "" {
				ws = append(ws, z.Name+": "+z.Warning)
			}
		}
		for _, h := range st.Hosts {
			if h.Error != "" {
				ws = append(ws, h.Host+": "+h.Error)
			}
		}
	}
	for _, r := range rows {
		switch {
		case r.Status.State == supervisor.Crashed:
			ws = append(ws, r.Name+" has crashed and is not being restarted: "+r.Status.LastError)
		case r.NeedsRestart:
			ws = append(ws, r.Name+" has configuration changes; restart it to apply them.")
		}
	}
	return ws
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	rows, err := s.appRows(r.Context())
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err)
		return
	}
	data := map[string]any{"Apps": rows, "Host": hostinfo.Read(s.Layout.Root), "Warnings": s.warnings(r.Context(), rows)}
	s.render(w, r, http.StatusOK, "home", "Apps", "apps", data)
}

// ---------------------------------------------------------------- apps

func formInput(r *http.Request) apps.Input {
	f := func(k string) string { return r.PostFormValue(k) }
	return apps.Input{
		Name: f("name"), Type: f("type"), Repo: f("repo"), Branch: f("branch"), Path: f("path"), Domain: f("domain"),
		Memory: f("memory"), CPU: f("cpu"), Pids: f("pids"), BuildMemory: f("build_memory"), BuildTimeout: f("build_timeout"),
	}
}

func (s *Server) newAppPage(w http.ResponseWriter, r *http.Request) {
	in := apps.Input{Type: "zig", Branch: "main", Memory: "256M", CPU: "1", Pids: "256", BuildMemory: "1G", BuildTimeout: "15m"}
	s.render(w, r, http.StatusOK, "app_new", "Add app", "apps", s.newAppData(r.Context(), in, nil))
}

func (s *Server) newAppData(ctx context.Context, in apps.Input, err error) map[string]any {
	d := map[string]any{"In": in}
	if err != nil {
		d["Errors"] = strings.Split(err.Error(), "\n")
	}
	// Offer the token's repositories (best effort, short timeout).
	if tok, ok := s.githubToken(ctx); ok {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		if repos, err := (&github.API{Token: tok}).Repos(cctx); err == nil {
			d["Repos"] = repos
		}
	}
	return d
}

func (s *Server) githubToken(ctx context.Context) (string, bool) {
	tok, err := s.Dep.GitHubToken(ctx)
	return tok, err == nil && tok != ""
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	in := formInput(r)
	a, err := s.Apps.Create(r.Context(), in)
	if err != nil {
		s.render(w, r, http.StatusUnprocessableEntity, "app_new", "Add app", "apps", s.newAppData(r.Context(), in, err))
		return
	}
	msg := "App created. Add env vars if it needs any, then press Deploy."
	if a.Domain != "" && s.Edge != nil {
		msg += " DNS and the certificate for " + a.Domain + " are being set up in the background."
	}
	redirect(w, r, "/apps/"+a.Name, nil, msg)
}

func (s *Server) appPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	rows, err := s.appRows(r.Context())
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err)
		return
	}
	var row *AppRow
	for i := range rows {
		if rows[i].Name == name {
			row = &rows[i]
		}
	}
	if row == nil {
		s.errorPage(w, r, http.StatusNotFound, fmt.Errorf("app %q not found", name))
		return
	}
	data := map[string]any{"App": row}
	if !row.Static {
		full, err := s.Apps.Get(r.Context(), name)
		if err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, err)
			return
		}
		row.App = full
		data["Releases"], _ = s.Dep.Releases(r.Context(), name)
		deps, _ := s.Dep.Deployments(r.Context(), name, 10)
		data["Deployments"] = deps
		data["Edit"] = apps.Input{
			Repo: full.Repo, Branch: full.Branch, Path: full.Path, Domain: full.Domain,
			Memory: humanLimit(full.Limits.MemoryMax), CPU: strconv.FormatFloat(full.Limits.CPUMax, 'f', -1, 64),
			Pids: strconv.Itoa(full.Limits.PidsMax), BuildMemory: humanLimit(full.BuildMemory), BuildTimeout: full.BuildTimeout.String(),
		}
	}
	if s.Edge != nil && row.Domain != "" {
		for _, h := range s.Edge.Status(r.Context()).Hosts {
			if h.Host == row.Domain {
				data["EdgeHost"] = h
			}
		}
	}
	s.render(w, r, http.StatusOK, "app", name, "apps", data)
}

func humanLimit(n int64) string {
	switch {
	case n%(1<<30) == 0:
		return strconv.FormatInt(n>>30, 10) + "G"
	case n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + "M"
	}
	return strconv.FormatInt(n, 10)
}

func (s *Server) deployApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	id, err := s.Dep.Deploy(r.Context(), name)
	if err != nil {
		redirect(w, r, "/apps/"+name, err, "")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/deployments/%d", id), http.StatusSeeOther)
}

func (s *Server) rollbackApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	id, err := s.Dep.Rollback(r.Context(), name, r.PostFormValue("release"))
	if err != nil {
		redirect(w, r, "/apps/"+name, err, "")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/deployments/%d", id), http.StatusSeeOther)
}

func (s *Server) appAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	action := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var err error
	switch action {
	case "start":
		err = s.Dep.Start(ctx, name)
	case "stop":
		err = s.Dep.StopApp(ctx, name)
	case "restart":
		err = s.Dep.Restart(ctx, name)
	}
	redirect(w, r, "/apps/"+name, err, name+": "+action+" done")
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	_, err := s.Apps.Update(r.Context(), name, formInput(r))
	msg := "Settings saved. Build settings apply to the next deploy; limits and the domain need a restart."
	redirect(w, r, "/apps/"+name, err, msg)
}

func (s *Server) setEnv(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	key := strings.TrimSpace(r.PostFormValue("key"))
	err := s.Apps.SetEnv(r.Context(), name, key, r.PostFormValue("value"))
	redirect(w, r, "/apps/"+name+"#env", err, key+" saved (encrypted). Restart the app to apply it.")
}

func (s *Server) deleteEnv(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	key := r.PostFormValue("key")
	err := s.Apps.DeleteEnv(r.Context(), name, key)
	redirect(w, r, "/apps/"+name+"#env", err, key+" removed. Restart the app to apply it.")
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if r.PostFormValue("confirm") != name {
		redirect(w, r, "/apps/"+name+"#danger", errors.New("type the app name exactly to confirm deletion"), "")
		return
	}
	res, err := s.Apps.Delete(r.Context(), name, r.PostFormValue("keep_data") == "1")
	if err != nil {
		redirect(w, r, "/apps/"+name+"#danger", err, "")
		return
	}
	msg := name + " deleted."
	if res.KeptData != "" {
		msg += " Its data was kept in " + res.KeptData + "."
	}
	if len(res.Warnings) > 0 {
		msg += " Some cleanup steps failed: " + strings.Join(res.Warnings, "; ")
	}
	redirect(w, r, "/", nil, msg)
}

// ---------------------------------------------------------------- logs & deployments

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if s.Sup.Get(name) == nil {
		s.errorPage(w, r, http.StatusNotFound, fmt.Errorf("app %q not found", name))
		return
	}
	s.render(w, r, http.StatusOK, "logs", name+" logs", "apps", map[string]any{"Name": name})
}

func (s *Server) deploymentPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, errNotFound)
		return
	}
	d, err := s.Dep.Deployment(r.Context(), id)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, err)
		return
	}
	data := map[string]any{"D": d}
	if d.Done() {
		data["Log"] = readLog(s.Layout.BuildLog(d.App, d.ID))
	}
	s.render(w, r, http.StatusOK, "deployment", fmt.Sprintf("Deployment #%d", d.ID), "apps", data)
}

// ---------------------------------------------------------------- settings

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := map[string]any{}
	if _, ok, _ := s.Store.GetSetting(ctx, deployer.SettingGitHubToken); ok {
		login, _, _ := s.Store.GetSetting(ctx, settingGitHubLogin)
		data["GitHubSet"], data["GitHubLogin"] = true, string(login)
	}
	if s.Edge != nil {
		data["Edge"] = s.Edge.Status(ctx)
	}
	tcs, _ := s.Zig.List()
	pins := map[string][]string{}
	for _, name := range s.Dep.Apps() {
		if rs, err := s.Dep.Releases(ctx, name); err == nil {
			for _, rel := range rs {
				if rel.Current {
					pins[rel.ZigVersion] = append(pins[rel.ZigVersion], name)
				}
			}
		}
	}
	type tc struct {
		toolchain.Installed
		UsedBy []string
	}
	var list []tc
	for _, t := range tcs {
		list = append(list, tc{t, pins[t.Version]})
	}
	data["Toolchains"] = list
	s.render(w, r, http.StatusOK, "settings", "Settings", "settings", data)
}

func (s *Server) setGitHubToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok := strings.TrimSpace(r.PostFormValue("token"))
	if r.PostFormValue("remove") == "1" {
		err := s.Dep.SetGitHubToken(ctx, "")
		s.Store.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, settingGitHubLogin)
		redirect(w, r, "/settings", err, "GitHub token removed.")
		return
	}
	if tok == "" {
		redirect(w, r, "/settings", errors.New("paste a token first"), "")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info, err := (&github.API{Token: tok}).Validate(cctx)
	if err != nil {
		redirect(w, r, "/settings", fmt.Errorf("token not saved: %w", err), "")
		return
	}
	if err := s.Dep.SetGitHubToken(ctx, tok); err != nil {
		redirect(w, r, "/settings", err, "")
		return
	}
	s.Store.SetSetting(ctx, settingGitHubLogin, []byte(info.Login))
	redirect(w, r, "/settings", nil, "GitHub token saved (encrypted). It belongs to "+info.Login+".")
}

func (s *Server) needEdge(w http.ResponseWriter, r *http.Request) bool {
	if s.Edge == nil {
		redirect(w, r, "/settings", errors.New(`the edge is disabled (edge.listen = "off")`), "")
		return false
	}
	return true
}

func (s *Server) setCloudflareToken(w http.ResponseWriter, r *http.Request) {
	if !s.needEdge(w, r) {
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	zones, err := s.Edge.SetToken(cctx, strings.TrimSpace(r.PostFormValue("token")))
	if err != nil {
		redirect(w, r, "/settings", fmt.Errorf("token not saved: %w", err), "")
		return
	}
	s.Edge.SyncInBackground()
	redirect(w, r, "/settings", nil, "Cloudflare token saved (encrypted). Zones: "+strings.Join(zones, ", ")+". Syncing DNS and certificates now.")
}

func (s *Server) edgeSync(w http.ResponseWriter, r *http.Request) {
	if !s.needEdge(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	_, err := s.Edge.Sync(ctx)
	redirect(w, r, "/settings", err, "Cloudflare sync finished without errors.")
}

func (s *Server) sslStrict(w http.ResponseWriter, r *http.Request) {
	if !s.needEdge(w, r) {
		return
	}
	zone := r.PostFormValue("zone")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	err := s.Edge.SetStrict(ctx, zone)
	redirect(w, r, "/settings", err, zone+": SSL/TLS mode set to Full (strict).")
}

func (s *Server) deleteToolchain(w http.ResponseWriter, r *http.Request) {
	v := r.PostFormValue("version")
	err := s.Zig.Delete(v)
	redirect(w, r, "/settings", err, "Zig "+v+" deleted. It is downloaded again when a deploy needs it.")
}

// ---------------------------------------------------------------- account

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	sessions, _ := s.Auth.Sessions(r.Context(), session(r))
	s.render(w, r, http.StatusOK, "account", "Account", "account", map[string]any{"Sessions": sessions})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("new") != r.PostFormValue("confirm") {
		redirect(w, r, "/account", errors.New("the new passwords do not match"), "")
		return
	}
	err := s.Auth.ChangePassword(r.Context(), session(r), r.PostFormValue("current"), r.PostFormValue("new"), edge.ClientIP(r))
	redirect(w, r, "/account", err, "Password changed. All other sessions were signed out.")
}

func (s *Server) revokeOthers(w http.ResponseWriter, r *http.Request) {
	n, err := s.Auth.RevokeOthers(r.Context(), session(r))
	redirect(w, r, "/account", err, fmt.Sprintf("Signed out %d other session(s).", n))
}
