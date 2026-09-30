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
	"github.com/sumitwaani2/dootd/internal/backup"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/github"
	"github.com/sumitwaani2/dootd/internal/hostinfo"
	"github.com/sumitwaani2/dootd/internal/metrics"
	"github.com/sumitwaani2/dootd/internal/supervisor"
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
	s.render(w, r, http.StatusOK, "login", "Sign in", "", s.loginData(r, map[string]any{"Next": safeNext(r.URL.Query().Get("next"))}))
}

// loginData adds what the sign-in page needs to know about the setup state.
func (s *Server) loginData(r *http.Request, data map[string]any) map[string]any {
	if _, err := s.Auth.Admin(r.Context()); errors.Is(err, auth.ErrNoAdmin) {
		data["NoAdmin"] = true
	}
	data["SetupPending"] = s.Auth.SetupUnused()
	data["OneTimeOnly"] = s.oneTimeOnly(r)
	data["Dashboard"] = s.Edge.DashboardHost()
	return data
}

// oneTimeOnly: on the setup address, once the dashboard domain works, only
// the one-time password is accepted (the admin password never travels
// outside Cloudflare).
func (s *Server) oneTimeOnly(r *http.Request) bool {
	return edge.IsDirect(r) && s.Edge.DashboardReady()
}

// safeNext only allows local absolute paths as redirect targets.
func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.ContainsAny(n, "\\\r\n") {
		return "/"
	}
	return n
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	email, next := strings.TrimSpace(r.FormValue("email")), safeNext(r.FormValue("next"))
	ip := edge.ClientIP(r)
	var tok string
	var err error
	oneTime := email == "" || s.oneTimeOnly(r)
	if oneTime {
		tok, err = s.Auth.LoginSetup(r.Context(), r.FormValue("password"), ip, r.UserAgent())
		next = "/setup"
	} else {
		tok, err = s.Auth.Login(r.Context(), email, r.FormValue("password"), ip, r.UserAgent())
	}
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrRateLimited) {
			status = http.StatusTooManyRequests
		} else if !errors.Is(err, auth.ErrInvalidLogin) && !errors.Is(err, auth.ErrNoAdmin) && !errors.Is(err, auth.ErrSetupInvalid) {
			s.Log.Error("login", "err", err)
			err = errors.New("sign-in failed; see the dootd logs")
			status = http.StatusInternalServerError
		}
		s.Log.Warn("failed sign-in", "ip", ip, "one_time", oneTime, "reason", err)
		s.render(w, r, status, "login", "Sign in", "", s.loginData(r, map[string]any{"Error": sentence(err.Error()), "Email": email, "Next": next}))
		return
	}
	s.Log.Info("signed in", "ip", ip, "one_time", oneTime, "setup_address", edge.IsDirect(r))
	setSessionCookie(w, tok, int(auth.AbsoluteTimeout/time.Second))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	email, _ := s.Auth.Admin(r.Context())
	s.render(w, r, http.StatusOK, "setup", "Set up your account", "", map[string]any{"Email": email})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	email := r.PostFormValue("email")
	var err error
	var tok string
	if r.PostFormValue("password") != r.PostFormValue("confirm") {
		err = errors.New("the passwords do not match")
	} else {
		tok, err = s.Auth.CompleteSetup(r.Context(), session(r), email, r.PostFormValue("password"), edge.ClientIP(r), r.UserAgent())
	}
	if err != nil {
		s.render(w, r, http.StatusUnprocessableEntity, "setup", "Set up your account", "", map[string]any{"Email": email, "Error": sentence(err.Error())})
		return
	}
	s.Log.Info("admin account set up", "ip", edge.ClientIP(r))
	setSessionCookie(w, tok, int(auth.AbsoluteTimeout/time.Second))
	redirect(w, r, "/", nil, "Your account is set up. Next: Settings → Cloudflare token, then the dashboard domain.")
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
	BackupProblem string
	LastBackup    time.Time
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
		if s.Backups != nil {
			row.BackupProblem = s.Backups.Problem(ctx, a.Name)
			if b, ok := s.Backups.Latest(ctx, a.Name); ok && b.Status == backup.StatusOK {
				row.LastBackup = b.CreatedAt
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) warnings(r *http.Request, rows []AppRow) []string {
	ctx := r.Context()
	var ws []string
	st := s.Edge.Status(ctx)
	if !st.TokenSet {
		ws = append(ws, "Setup: no Cloudflare token is set, so DNS records and certificates cannot be managed. Add one in Settings → Cloudflare.")
	}
	problem := s.Edge.DashboardProblem()
	switch {
	case st.Dashboard == "":
		ws = append(ws, "Setup: no dashboard domain yet, so the dashboard is only reachable on the server's IP address. Set one in Settings → Dashboard domain.")
	case problem != "":
		ws = append(ws, "Setup: the dashboard domain "+st.Dashboard+" is not ready yet: "+problem+". Until it is ready the dashboard also answers on the server's IP address.")
	case edge.IsDirect(r):
		ws = append(ws, "You are on the setup address. The dashboard is at https://"+st.Dashboard+"/ and this address stops answering once the one-time password is used or expires.")
	}
	for _, z := range st.Zones {
		if z.Warning != "" && z.Warning != problem {
			ws = append(ws, z.Name+": "+z.Warning)
		}
	}
	for _, h := range st.Hosts {
		if h.Error != "" {
			ws = append(ws, h.Host+": "+h.Error)
		}
	}
	if _, ok, _ := s.Store.GetSetting(ctx, deployer.SettingGitHubToken); !ok {
		ws = append(ws, "No GitHub token is set, so only public repositories can be deployed. Add one in Settings → GitHub.")
	}
	if s.Backups != nil {
		if _, ok, _ := s.Backups.S3Config(ctx); !ok {
			ws = append(ws, "No S3 bucket is set, so backups are only kept on this server. Add R2 or another S3-compatible bucket in Settings.")
		}
	}
	ws = append(ws, s.metricWarnings(ctx, rows)...)
	for _, r := range rows {
		if r.BackupProblem != "" {
			ws = append(ws, r.Name+": "+r.BackupProblem)
		}
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
	data := map[string]any{"Apps": rows, "Host": hostinfo.Read(s.Layout.Root), "Warnings": s.warnings(r, rows)}
	s.render(w, r, http.StatusOK, "home", "Apps", "apps", data)
}

// ---------------------------------------------------------------- apps

func formInput(r *http.Request) apps.Input {
	f := func(k string) string { return r.PostFormValue(k) }
	return apps.Input{
		Repo: f("repo"), Domain: f("domain"),
		Memory: f("memory"), CPU: f("cpu"), Pids: f("pids"),
		RestoreFrom: f("restore_from"),
	}
}

func (s *Server) newAppPage(w http.ResponseWriter, r *http.Request) {
	in := apps.Input{Memory: "256M", CPU: "1", Pids: "256", RestoreFrom: apps.AutoFolder}
	s.render(w, r, http.StatusOK, "app_new", "Add app", "apps", s.newAppData(r.Context(), in, nil))
}

func (s *Server) newAppData(ctx context.Context, in apps.Input, err error) map[string]any {
	d := map[string]any{"In": in}
	if err != nil {
		var list []string
		for _, e := range strings.Split(err.Error(), "\n") {
			list = append(list, sentence(e))
		}
		d["Errors"] = list
	}
	// Offer the bucket's backup folders (docs/architecture.md).
	fctx, fcancel := context.WithTimeout(ctx, 10*time.Second)
	defer fcancel()
	folders, ok, ferr := s.Backups.Folders(fctx)
	d["BucketSet"], d["Folders"] = ok, folders
	if ferr != nil {
		d["FoldersErr"] = ferr.Error()
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
	msg := "App " + a.Name + " created."
	switch {
	case a.RestoreErr != nil:
		err = fmt.Errorf("app %s created, but its data could not be restored from the backup folder %s/ (the app starts with an empty DATA_DIR): %w", a.Name, a.RestoredFrom, a.RestoreErr)
	case a.RestoredFrom != "":
		msg += fmt.Sprintf(" Its data was restored from the newest backup in %s/ (%s).", a.RestoredFrom, strings.Join(a.Restored, ", "))
	}
	msg += " Add env vars if it needs any, then press Deploy."
	if a.Domain != "" {
		msg += " DNS and the certificate for " + a.Domain + " are being set up in the background."
	}
	redirect(w, r, "/apps/"+a.Name, err, msg)
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
	rng := metrics.ParseRange(r.URL.Query().Get("range"))
	data := map[string]any{"App": row, "Range": string(rng), "Ranges": []string{"1h", "24h", "7d"},
		"Charts": chartRefs(name, rng, [][2]string{{"cpu", "CPU"}, {"memory", "Memory"}, {"requests", "Requests"}, {"latency", "Response time"}})}
	if s.Metrics != nil {
		if p, ok := s.Metrics.Latest(name); ok {
			data["Now"] = p
		}
	}
	{
		full, err := s.Apps.Get(r.Context(), name)
		if err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, err)
			return
		}
		row.App = full
		data["Releases"], _ = s.Dep.Releases(r.Context(), name)
		gh, err := s.Dep.GitHubReleases(r.Context(), name)
		data["GitHubReleases"] = gh
		if err != nil {
			data["GitHubErr"] = err.Error()
		}
		deps, _ := s.Dep.Deployments(r.Context(), name, 10)
		data["Deployments"] = deps
		if s.Backups != nil {
			data["Backups"], _ = s.Backups.List(r.Context(), name)
			_, data["S3Set"], _ = s.Backups.S3Config(r.Context())
			data["Policy"] = s.policyText()
		}
		data["Edit"] = apps.Input{
			Repo: full.Repo, Domain: full.Domain,
			Memory: humanLimit(full.Limits.MemoryMax), CPU: strconv.FormatFloat(full.Limits.CPUMax, 'f', -1, 64),
			Pids: strconv.Itoa(full.Limits.PidsMax),
		}
	}
	if row.Domain != "" {
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
	id, err := s.Dep.Deploy(r.Context(), name, r.PostFormValue("tag"))
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
	msg := "Settings saved. Limits need a restart; the domain is re-routed now."
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
	res, err := s.Apps.Delete(r.Context(), name, r.PostFormValue("keep_data") == "1", r.PostFormValue("delete_backups") == "1")
	if err != nil {
		redirect(w, r, "/apps/"+name+"#danger", err, "")
		return
	}
	msg := name + " deleted."
	if res.KeptData != "" {
		msg += " Its data was kept in " + res.KeptData + "."
	}
	if res.DeletedBackups {
		msg += " Its backups were deleted."
	} else if r.PostFormValue("delete_backups") != "1" {
		msg += " Its backups were kept."
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
	data["Edge"] = s.Edge.Status(ctx)
	data["DashboardProblem"] = s.Edge.DashboardProblem()
	data["DashboardReady"] = data["DashboardProblem"] == ""
	if s.Backups != nil {
		if c, ok, err := s.Backups.S3Config(ctx); err == nil && ok {
			c.SecretKey = ""
			data["S3"], data["S3Set"] = c, true
		}
		data["Policy"] = s.policyText()
	}
	s.render(w, r, http.StatusOK, "settings", "Settings", "settings", data)
}

func (s *Server) setGitHubToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok := strings.TrimSpace(r.PostFormValue("token"))
	if r.PostFormValue("remove") == "1" {
		err := s.Dep.SetGitHubToken(ctx, "")
		s.Dep.ForgetReleases("")
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
	s.Dep.ForgetReleases("")
	redirect(w, r, "/settings", nil, "GitHub token saved (encrypted). It belongs to "+info.Login+".")
}

func (s *Server) setCloudflareToken(w http.ResponseWriter, r *http.Request) {
	cctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	zones, missing, err := s.Edge.SetToken(cctx, strings.TrimSpace(r.PostFormValue("token")))
	if err != nil {
		redirect(w, r, "/settings", fmt.Errorf("token not saved: %w", err), "")
		return
	}
	s.Edge.SyncInBackground()
	if len(missing) > 0 {
		redirect(w, r, "/settings", fmt.Errorf("the Cloudflare token was saved (encrypted), but it lacks these permissions: %s. "+
			"Edit the token in Cloudflare (My Profile → API Tokens) to add them, then press Sync now", strings.Join(missing, ", ")), "")
		return
	}
	redirect(w, r, "/settings", nil, "Cloudflare token saved (encrypted). Zones: "+strings.Join(zones, ", ")+". Syncing DNS and certificates now.")
}

func (s *Server) edgeSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	_, err := s.Edge.Sync(ctx)
	redirect(w, r, "/settings", err, "Cloudflare sync finished without errors.")
}

func (s *Server) sslStrict(w http.ResponseWriter, r *http.Request) {
	zone := r.PostFormValue("zone")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	err := s.Edge.SetStrict(ctx, zone)
	redirect(w, r, "/settings", err, zone+": SSL/TLS mode set to Full (strict).")
}

func (s *Server) setDashboardDomain(w http.ResponseWriter, r *http.Request) {
	old := s.Edge.DashboardHost()
	domain := strings.ToLower(strings.TrimSpace(r.PostFormValue("domain")))
	for _, a := range s.Apps.Routes() {
		if a.Host == domain {
			redirect(w, r, "/settings#dashboard-domain", fmt.Errorf("%s is the domain of the app %s", domain, a.App), "")
			return
		}
	}
	if err := s.Edge.SetDashboardHost(r.Context(), domain); err != nil {
		redirect(w, r, "/settings#dashboard-domain", fmt.Errorf("dashboard domain not saved: %w", err), "")
		return
	}
	// On the old domain the next page would not be routed to the dashboard
	// any more, so say where to go instead of redirecting.
	if !edge.IsDirect(r) && old != "" && old != domain {
		s.render(w, r, http.StatusOK, "notice", "Dashboard domain changed", "", map[string]any{
			"Heading": "The dashboard is moving to " + domain,
			"Message": "The DNS record and certificate are being set up; this usually takes under a minute. Then sign in again there.",
			"Link":    "https://" + domain + "/",
		})
		return
	}
	redirect(w, r, "/settings#dashboard-domain", nil, "Dashboard domain "+domain+" saved. DNS record, certificate and origin pulls are being set up; this page shows when it is ready.")
}

// ---------------------------------------------------------------- account

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	sessions, _ := s.Auth.Sessions(r.Context(), session(r))
	s.render(w, r, http.StatusOK, "account", "Account", "account", map[string]any{"Sessions": sessions})
}

func (s *Server) changeEmail(w http.ResponseWriter, r *http.Request) {
	err := s.Auth.ChangeEmail(r.Context(), r.PostFormValue("current"), r.PostFormValue("email"), edge.ClientIP(r))
	redirect(w, r, "/account", err, "Email changed.")
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
