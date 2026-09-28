// Package apps is the app registry behind the dashboard: apps and their
// env vars are stored in dootd.db and wired into the deployer, the
// supervisor and the edge router.
package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sumitwaani2/dootd/internal/app"
	"github.com/sumitwaani2/dootd/internal/builder"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/github"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
	"github.com/sumitwaani2/dootd/internal/users"
)

// Port range for app listeners.
const (
	firstPort = 20001
	lastPort  = 20999
	maxApps   = 20
)

// Edge is the part of the edge manager the registry uses (nil = no edge).
type Edge interface {
	SetRoutes([]edge.Route)
	SyncInBackground()
	RemoveHost(ctx context.Context, host string) error
}

// App is one app as configured in the dashboard.
type App struct {
	Name         string
	Type         app.Type
	Repo         string // as entered, normalized
	Branch       string
	Path         string
	Domain       string
	Port         int
	Limits       app.Limits
	BuildMemory  int64
	BuildTimeout time.Duration
	CreatedAt    time.Time
	EnvNames     []string
	Static       bool // defined in the test-only --dev-apps file
}

// Input is the form data for creating or editing an app. Empty optional
// fields mean "default" (create) or "unchanged" is never used: edits send
// every field.
type Input struct {
	Name, Type, Repo, Branch, Path, Domain string
	Memory, CPU, Pids                      string
	BuildMemory, BuildTimeout              string
}

// Service manages apps.
type Service struct {
	db            *store.Store
	box           *secrets.Box
	dep           *deployer.Deployer
	sup           *supervisor.Supervisor
	edge          Edge
	dashboardHost string
	log           *slog.Logger

	mu      sync.Mutex
	static  map[string]bool
	backups BackupDeleter
}

// BackupDeleter removes an app's backups (implemented by internal/backup).
type BackupDeleter interface {
	DeleteAll(ctx context.Context, app string) error
}

// SetBackups connects the backup service (used when deleting apps).
func (s *Service) SetBackups(b BackupDeleter) { s.backups = b }

// New creates the service. edge may be nil.
func New(db *store.Store, box *secrets.Box, dep *deployer.Deployer, sup *supervisor.Supervisor, e Edge, dashboardHost string, log *slog.Logger) *Service {
	if e != nil {
		// Avoid a typed-nil interface.
		if m, ok := e.(*edge.Manager); ok && m == nil {
			e = nil
		}
	}
	return &Service{db: db, box: box, dep: dep, sup: sup, edge: e, dashboardHost: dashboardHost, log: log, static: map[string]bool{}}
}

// MarkStatic records apps registered from the --dev-apps file.
func (s *Service) MarkStatic(name string) {
	s.mu.Lock()
	s.static[name] = true
	s.mu.Unlock()
}

func envPurpose(appName, key string) string { return "app_env:" + appName + ":" + key }

// Load registers every stored app. Apps that fail to load are logged and
// skipped so one bad row cannot keep dootd down.
func (s *Service) Load(ctx context.Context) error {
	list, err := s.stored(ctx)
	if err != nil {
		return err
	}
	for _, a := range list {
		cfg, err := s.config(ctx, a)
		if err == nil {
			_, err = s.dep.Register(ctx, cfg)
		}
		if err != nil {
			s.log.Error("app not loaded", "app", a.Name, "err", err)
			continue
		}
		s.log.Info("app loaded", "app", a.Name, "domain", a.Domain, "port", a.Port)
	}
	return nil
}

const appCols = `name, type, repo, branch, path, domain, port, memory_max, cpu_max, pids_max, build_memory, build_timeout, created_at`

func scanApp(r interface{ Scan(...any) error }) (App, error) {
	var a App
	var typ string
	var timeout, created int64
	err := r.Scan(&a.Name, &typ, &a.Repo, &a.Branch, &a.Path, &a.Domain, &a.Port,
		&a.Limits.MemoryMax, &a.Limits.CPUMax, &a.Limits.PidsMax, &a.BuildMemory, &timeout, &created)
	a.Type, a.BuildTimeout, a.CreatedAt = app.Type(typ), time.Duration(timeout)*time.Second, time.Unix(created, 0)
	return a, err
}

func (s *Service) stored(ctx context.Context) ([]App, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+appCols+` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get returns one stored app with its env var names.
func (s *Service) Get(ctx context.Context, name string) (App, error) {
	a, err := scanApp(s.db.Reader().QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("app %q not found", name)
	}
	if err != nil {
		return a, err
	}
	env, err := s.env(ctx, name)
	if err != nil {
		return a, err
	}
	for k := range env {
		a.EnvNames = append(a.EnvNames, k)
	}
	sort.Strings(a.EnvNames)
	return a, nil
}

// List returns stored apps plus static dev apps, sorted by name.
func (s *Service) List(ctx context.Context) ([]App, error) {
	list, err := s.stored(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for name := range s.static {
		if a := s.sup.Get(name); a != nil {
			sp := a.Spec()
			list = append(list, App{Name: sp.Name, Type: sp.Type, Domain: sp.Domain, Port: sp.Port, Limits: sp.Limits, Static: true})
		}
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (s *Service) env(ctx context.Context, name string) (map[string]string, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT name, value FROM app_env WHERE app = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	env := map[string]string{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		pt, err := s.box.Open(v, envPurpose(name, k))
		if err != nil {
			return nil, fmt.Errorf("env var %s of %s: %w", k, name, err)
		}
		env[k] = string(pt)
	}
	return env, rows.Err()
}

func (s *Service) config(ctx context.Context, a App) (deployer.AppConfig, error) {
	repo, err := github.ParseRepo(a.Repo)
	if err != nil {
		return deployer.AppConfig{}, err
	}
	env, err := s.env(ctx, a.Name)
	if err != nil {
		return deployer.AppConfig{}, err
	}
	return deployer.AppConfig{
		Base: app.Spec{
			Name: a.Name, Type: a.Type, Domain: a.Domain, Port: a.Port,
			HealthPath: app.DefaultHealthPath, Env: env, Limits: a.Limits,
		},
		Repo: repo, Branch: a.Branch, Subdir: a.Path,
		BuildMemory: a.BuildMemory, BuildTimeout: a.BuildTimeout,
	}, nil
}

// validate turns form input into an App, reporting every problem.
func (s *Service) validate(ctx context.Context, in Input, existing *App) (App, error) {
	var errs []error
	a := App{}
	if existing != nil {
		a = *existing
	} else {
		a.Name = strings.TrimSpace(in.Name)
		if err := app.ValidateName(a.Name); err != nil {
			errs = append(errs, err)
		}
		a.Type = app.Type(in.Type)
		if a.Type != app.TypeZig && a.Type != app.TypeC {
			errs = append(errs, errors.New("type must be zig or c"))
		}
	}
	repo, err := github.ParseRepo(in.Repo)
	if err != nil {
		errs = append(errs, err)
	} else if repo.GitHub {
		a.Repo = "https://github.com/" + repo.Owner + "/" + repo.Name
	} else {
		a.Repo = repo.URL
	}
	a.Branch = strings.TrimSpace(in.Branch)
	if a.Branch == "" {
		a.Branch = "main"
	}
	if err := github.ValidateBranch(a.Branch); err != nil {
		errs = append(errs, err)
	}
	if a.Path, err = app.CleanSubdir(in.Path); err != nil {
		errs = append(errs, fmt.Errorf("path: %w", err))
	}
	a.Domain = strings.ToLower(strings.TrimSpace(in.Domain))
	if a.Domain != "" {
		if err := edge.ValidHostname(a.Domain); err != nil {
			errs = append(errs, err)
		} else if a.Domain == s.dashboardHost {
			errs = append(errs, errors.New("that domain is used by the dashboard"))
		}
	}
	parse := func(field, v, def string, f func(string) error) {
		if strings.TrimSpace(v) == "" {
			v = def
		}
		if err := f(strings.TrimSpace(v)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
	}
	parse("memory limit", in.Memory, "256M", func(v string) (err error) {
		a.Limits.MemoryMax, err = app.ParseBytes(v)
		return
	})
	parse("CPU limit", in.CPU, "1", func(v string) (err error) {
		a.Limits.CPUMax, err = strconv.ParseFloat(v, 64)
		return
	})
	parse("process limit", in.Pids, strconv.Itoa(app.DefaultPidsMax), func(v string) (err error) {
		a.Limits.PidsMax, err = strconv.Atoi(v)
		return
	})
	parse("build memory", in.BuildMemory, "1G", func(v string) (err error) {
		a.BuildMemory, err = app.ParseBytes(v)
		if err == nil && a.BuildMemory < 128<<20 {
			err = errors.New("must be at least 128M")
		}
		return
	})
	parse("build timeout", in.BuildTimeout, "15m", func(v string) (err error) {
		a.BuildTimeout, err = time.ParseDuration(v)
		if err == nil && (a.BuildTimeout < 10*time.Second || a.BuildTimeout > 2*time.Hour) {
			err = errors.New("must be between 10s and 2h")
		}
		return
	})
	if len(errs) == 0 {
		spec := app.Spec{Name: a.Name, Type: a.Type, Port: firstPort, HealthPath: "/", Limits: a.Limits}
		if err := spec.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	// Uniqueness across stored and static apps.
	for _, other := range s.sup.Apps() {
		sp := other.Spec()
		if sp.Name == a.Name {
			if existing == nil {
				errs = append(errs, fmt.Errorf("an app named %q already exists", a.Name))
			}
			continue
		}
		if a.Domain != "" && sp.Domain == a.Domain {
			errs = append(errs, fmt.Errorf("domain %s is already used by %s", a.Domain, sp.Name))
		}
	}
	return a, errors.Join(errs...)
}

func (s *Service) freePort() (int, error) {
	used := map[int]bool{}
	for _, a := range s.sup.Apps() {
		used[a.Spec().Port] = true
	}
	for p := firstPort; p <= lastPort; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, errors.New("no free port")
}

// Create stores and registers a new app (Req 7.1, 7.2). It does not deploy.
func (s *Service) Create(ctx context.Context, in Input) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sup.Apps()) >= maxApps {
		return App{}, fmt.Errorf("at most %d apps are supported on one host", maxApps)
	}
	a, err := s.validate(ctx, in, nil)
	if err != nil {
		return a, err
	}
	if a.Port, err = s.freePort(); err != nil {
		return a, err
	}
	a.CreatedAt = time.Now()
	if _, err := s.db.Writer().ExecContext(ctx, `INSERT INTO apps (`+appCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Name, string(a.Type), a.Repo, a.Branch, a.Path, a.Domain, a.Port, a.Limits.MemoryMax, a.Limits.CPUMax,
		a.Limits.PidsMax, a.BuildMemory, int64(a.BuildTimeout/time.Second), a.CreatedAt.Unix()); err != nil {
		return a, fmt.Errorf("save app: %w", err)
	}
	cfg, err := s.config(ctx, a)
	if err == nil {
		_, err = s.dep.Register(ctx, cfg)
	}
	if err != nil {
		s.db.Writer().ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, a.Name)
		return a, err
	}
	s.log.Info("app created", "app", a.Name, "domain", a.Domain, "port", a.Port)
	s.refreshEdge(true)
	return a, nil
}

// Update changes an app's source, domain and limits. Running processes
// keep their old settings until restarted.
func (s *Service) Update(ctx context.Context, name string, in Input) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.Get(ctx, name)
	if err != nil {
		return cur, err
	}
	a, err := s.validate(ctx, in, &cur)
	if err != nil {
		return a, err
	}
	if _, err := s.db.Writer().ExecContext(ctx, `UPDATE apps SET repo = ?, branch = ?, path = ?, domain = ?, memory_max = ?,
		cpu_max = ?, pids_max = ?, build_memory = ?, build_timeout = ? WHERE name = ?`,
		a.Repo, a.Branch, a.Path, a.Domain, a.Limits.MemoryMax, a.Limits.CPUMax, a.Limits.PidsMax,
		a.BuildMemory, int64(a.BuildTimeout/time.Second), name); err != nil {
		return a, err
	}
	if err := s.pushConfig(ctx, a); err != nil {
		return a, err
	}
	if cur.Domain != a.Domain {
		s.refreshEdge(a.Domain != "")
		if cur.Domain != "" && s.edge != nil {
			if err := s.edge.RemoveHost(ctx, cur.Domain); err != nil {
				s.log.Warn("cleaning up the old domain", "app", name, "domain", cur.Domain, "err", err)
			}
		}
	}
	return a, nil
}

func (s *Service) pushConfig(ctx context.Context, a App) error {
	cfg, err := s.config(ctx, a)
	if err != nil {
		return err
	}
	return s.dep.UpdateConfig(cfg)
}

// SetEnv sets (or replaces) an env var (Req 7.3). A restart applies it.
func (s *Service) SetEnv(ctx context.Context, name, key, value string) error {
	key = strings.TrimSpace(key)
	if err := app.ValidateEnvName(key); err != nil {
		return err
	}
	if len(value) > 64<<10 {
		return errors.New("value is larger than 64 KB")
	}
	a, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if len(a.EnvNames) >= 200 {
		return errors.New("too many env vars")
	}
	sealed, err := s.box.Seal([]byte(value), envPurpose(name, key))
	if err != nil {
		return err
	}
	if _, err := s.db.Writer().ExecContext(ctx, `INSERT INTO app_env (app, name, value) VALUES (?, ?, ?)
		ON CONFLICT (app, name) DO UPDATE SET value = excluded.value`, name, key, sealed); err != nil {
		return err
	}
	return s.pushConfig(ctx, a)
}

// DeleteEnv removes an env var.
func (s *Service) DeleteEnv(ctx context.Context, name, key string) error {
	a, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if _, err := s.db.Writer().ExecContext(ctx, `DELETE FROM app_env WHERE app = ? AND name = ?`, name, key); err != nil {
		return err
	}
	return s.pushConfig(ctx, a)
}

// DeleteResult reports what Delete did.
type DeleteResult struct {
	KeptData       string   // where DATA_DIR was moved, if kept
	DeletedBackups bool     // local and remote backups removed
	Warnings       []string // cleanup steps that failed
}

// Delete stops and removes an app (Req 7.4): process, cgroup, user,
// releases, logs, caches, routing, DNS record and certificate. DATA_DIR is
// moved aside if keepData is set, otherwise deleted. Backups are kept
// unless deleteBackups is set.
func (s *Service) Delete(ctx context.Context, name string, keepData, deleteBackups bool) (DeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res DeleteResult
	a, err := s.Get(ctx, name)
	if err != nil {
		return res, err
	}
	if err := s.dep.Unregister(name); err != nil {
		return res, err
	}
	warn := func(step string, err error) {
		if err != nil {
			res.Warnings = append(res.Warnings, step+": "+err.Error())
			s.log.Warn("app delete: "+step, "app", name, "err", err)
		}
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := s.sup.Remove(sctx, name); err != nil {
		// Put it back so it is not half-deleted.
		if cfg, cerr := s.config(ctx, a); cerr == nil {
			s.dep.UpdateConfig(cfg)
		}
		return res, err
	}
	if _, err := s.db.Writer().ExecContext(sctx, `DELETE FROM apps WHERE name = ?`, name); err != nil {
		return res, err
	}
	warn("history", s.dep.Forget(sctx, name))
	s.refreshEdge(false)
	if a.Domain != "" && s.edge != nil {
		warn("domain cleanup", s.edge.RemoveHost(sctx, a.Domain))
	}
	kept, err := s.dep.PurgeFiles(name, keepData, time.Now().Unix())
	warn("files", err)
	res.KeptData = kept
	warn("build cgroup", builder.RemoveGroup(s.dep.Builder, name))
	warn("system user", users.Remove(name))
	if deleteBackups && s.backups != nil {
		err := s.backups.DeleteAll(sctx, name)
		warn("backups", err)
		res.DeletedBackups = err == nil
	}
	s.log.Info("app deleted", "app", name, "kept_data", kept)
	return res, nil
}

// Routes returns the edge routes of every app with a domain.
func (s *Service) Routes() []edge.Route {
	var rs []edge.Route
	for _, a := range s.sup.Apps() {
		sp := a.Spec()
		if cfg, ok := s.dep.Config(sp.Name); ok {
			sp = cfg.Base // domain edits apply to routing immediately
		}
		if sp.Domain != "" {
			rs = append(rs, edge.Route{Host: sp.Domain, App: sp.Name, Port: sp.Port})
		}
	}
	return rs
}

func (s *Service) refreshEdge(sync bool) {
	if s.edge == nil {
		return
	}
	s.edge.SetRoutes(s.Routes())
	if sync {
		s.edge.SyncInBackground()
	}
}

// RefreshEdge pushes the routing table to the edge (used at startup).
func (s *Service) RefreshEdge() { s.refreshEdge(false) }
