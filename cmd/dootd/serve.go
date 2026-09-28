package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/sumitwaani2/dootd/internal/apps"
	"github.com/sumitwaani2/dootd/internal/auth"
	"github.com/sumitwaani2/dootd/internal/backup"
	"github.com/sumitwaani2/dootd/internal/builder"
	"github.com/sumitwaani2/dootd/internal/buildinfo"
	"github.com/sumitwaani2/dootd/internal/cgroup"
	"github.com/sumitwaani2/dootd/internal/config"
	"github.com/sumitwaani2/dootd/internal/control"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/metrics"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
	"github.com/sumitwaani2/dootd/internal/toolchain"
	"github.com/sumitwaani2/dootd/internal/users"
	"github.com/sumitwaani2/dootd/internal/web"
)

func runServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", config.DefaultPath, "path to config.toml")
	devApps := fs.String("dev-apps", "", "TEMPORARY (until the dashboard exists): TOML file listing apps")
	socket := fs.String("socket", control.DefaultSocket, "control socket for `dootd ctl`")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfgExplicit := false
	fs.Visit(func(f *flag.Flag) { cfgExplicit = cfgExplicit || f.Name == "config" })

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := serve(log, *cfgPath, cfgExplicit, *devApps, *socket); err != nil {
		log.Error("dootd stopped with an error", "err", err)
		return 1
	}
	return 0
}

func serve(log *slog.Logger, cfgPath string, cfgExplicit bool, devApps, socket string) error {
	if os.Geteuid() != 0 {
		return errors.New("dootd serve must run as root (it manages users and cgroups)")
	}
	log.Info("starting", "version", buildinfo.String())

	cfg, err := config.Load(cfgPath, cfgExplicit)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	box, err := secrets.LoadOrCreate(cfg.MasterKey)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := os.Chmod(cfg.DBPath(), 0o600); err != nil {
		return err
	}
	ver, _ := st.SchemaVersion(ctx)
	log.Info("state database ready", "path", cfg.DBPath(), "schema", ver)

	cg, err := cgroup.Setup(cfg.CgroupRoot)
	if err != nil {
		return err
	}
	log.Info("cgroup tree ready", "root", cg.Root(), "controllers", keys(cg.Controllers()))
	if cleaned, err := cg.ReapStale(ctx); err != nil {
		return err
	} else if len(cleaned) > 0 {
		log.Warn("killed leftover processes from a previous run", "groups", cleaned)
	}

	lay := layout.Layout{Root: cfg.DataRoot}
	sup := supervisor.New(supervisor.Deps{Layout: lay, Cgroups: cg, Log: log})
	dep, err := deployer.New(ctx, deployer.Deps{
		Layout: lay, Store: st, Secrets: box, Supervisor: sup,
		Builder: &builder.Builder{Layout: lay, Cgroups: cg},
		Zig:     toolchain.New(lay.ToolchainsDir()),
		Log:     log,
	})
	if err != nil {
		return err
	}

	// Test-only prebuilt/deployable apps from a file (scripts/e2e).
	var static []string
	if devApps != "" {
		list, err := config.LoadDevApps(devApps)
		if err != nil {
			return err
		}
		for _, la := range list {
			if err := register(ctx, log, sup, dep, la); err != nil {
				return err
			}
			static = append(static, la.Spec.Name)
		}
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var edgeMgr *edge.Manager
	if cfg.Edge.Listen != "off" {
		if edgeMgr, err = newEdge(ctx, log, cfg, st, box, sup, dep); err != nil {
			return err
		}
	} else {
		log.Warn("edge disabled (edge.listen = \"off\"): apps and the dashboard are only reachable on 127.0.0.1")
	}

	bk := newBackups(lay, st, box, sup, dep, log)
	bk.Interval, bk.Retention = cfg.Backups.Interval.Duration, cfg.Backups.Retention.Duration
	iv, rt := bk.Policy()
	log.Info("backups", "every", iv, "keep", rt)
	if err := bk.Init(ctx); err != nil {
		return err
	}
	dep.PreDeploy = func(ctx context.Context, name string, lg *logs.Log) error {
		_, err := bk.Run(ctx, name, backup.KindPreDeploy, false, lg)
		if errors.Is(err, backup.ErrNoDatabases) {
			lg.Writef("pre-deploy backup: no SQLite databases in DATA_DIR yet; nothing to back up")
			return nil
		}
		return err
	}

	appSvc := apps.New(st, box, dep, sup, edgeMgr, cfg.Edge.DashboardDomain, log)
	appSvc.SetBackups(bk)
	for _, n := range static {
		appSvc.MarkStatic(n)
	}
	if err := appSvc.Load(ctx); err != nil {
		return err
	}
	authSvc := auth.New(st)

	mc := &metrics.Collector{Store: st, Sup: sup, DataRoot: cfg.DataRoot, Log: log.With("component", "metrics")}
	if edgeMgr != nil {
		mc.Requests = edgeMgr.Router.Stats
	}

	edgeErr := make(chan error, 1)
	if edgeMgr != nil {
		for _, rt := range appSvc.Routes() {
			if rt.Host == cfg.Edge.DashboardDomain {
				return fmt.Errorf("app %s uses the dashboard domain %s", rt.App, rt.Host)
			}
		}
		dash := &web.Server{
			Auth: authSvc, Apps: appSvc, Dep: dep, Sup: sup, Edge: edgeMgr, Zig: dep.Zig, Store: st,
			Backups: bk, MasterKeyPath: cfg.MasterKey, Metrics: mc,
			Thresholds: web.Thresholds{DiskPercent: cfg.Monitoring.DiskWarnPercent,
				MemoryPercent: cfg.Monitoring.MemoryWarnPercent, CertDays: cfg.Monitoring.CertWarnDays},
			Layout: lay, Host: cfg.Edge.DashboardDomain, Version: buildinfo.Version, Log: log.With("component", "web"),
		}
		h, err := dash.Handler()
		if err != nil {
			return err
		}
		if cfg.Edge.DashboardDomain == "" {
			log.Warn("no edge.dashboard_domain configured: the dashboard is not served")
		} else {
			edgeMgr.Router.Dashboard = h
			if _, err := authSvc.Admin(ctx); errors.Is(err, auth.ErrNoAdmin) {
				log.Warn("no dashboard admin yet; create it with: sudo dootd ctl admin set-password --email you@example.com")
			}
		}
		appSvc.RefreshEdge()
		go func() { edgeErr <- edgeMgr.Serve(runCtx) }()
		edgeMgr.Run(runCtx)
	}

	ctl := &control.Server{Sup: sup, Dep: dep, Edge: edgeMgr, Auth: authSvc, Backups: bk, Metrics: mc, Layout: lay, Log: log}
	ctlErr := make(chan error, 1)
	go func() { ctlErr <- ctl.Serve(runCtx, socket) }()
	log.Info("control socket ready", "path", socket)

	dep.Run(runCtx)
	bk.Schedule(runCtx)
	mc.Run(runCtx)
	go sup.StartAll(runCtx, func(name string) bool { return dep.DesiredRunning(runCtx, name) })

	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)

loop:
	for {
		select {
		case <-usr1:
			logStatus(log, sup)
		case err := <-edgeErr:
			if err != nil {
				stop()
				dep.Stop()
				sup.StopAll(context.Background())
				return err
			}
		case err := <-ctlErr:
			if err != nil {
				log.Error("control socket failed", "err", err)
			}
			if runCtx.Err() != nil {
				break loop
			}
		case <-runCtx.Done():
			break loop
		}
	}

	log.Info("shutting down: cancelling deployments")
	dep.Stop()
	waitUploads(log, bk, 20*time.Second)
	log.Info("stopping apps")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.StopAll(stopCtx); err != nil {
		return fmt.Errorf("stopping apps: %w", err)
	}
	os.Remove(socket)
	log.Info("all apps stopped; bye")
	return nil
}

// newBackups creates the backup service and connects it to the apps.
func newBackups(lay layout.Layout, st *store.Store, box *secrets.Box, sup *supervisor.Supervisor, dep *deployer.Deployer, log *slog.Logger) *backup.Service {
	return &backup.Service{
		Layout: lay, Store: st, Box: box, Log: log.With("component", "backup"),
		SelfSnapshot: func(ctx context.Context, dst string) error {
			_, err := st.Writer().ExecContext(ctx, `VACUUM INTO ?`, dst)
			return err
		},
		Hooks: backup.Hooks{
			Apps: func() []string {
				var names []string
				for _, a := range sup.Apps() {
					names = append(names, a.Spec().Name)
				}
				return names
			},
			Release: func(name string) string {
				if a := sup.Get(name); a != nil {
					return a.Status().Release
				}
				return ""
			},
			Reserve: func(name string) (func(), error) { return dep.Reserve(name, "a restore") },
			Stop: func(ctx context.Context, name string) (bool, error) {
				a := sup.Get(name)
				if a == nil {
					return false, fmt.Errorf("unknown app %q", name)
				}
				switch a.Status().State {
				case supervisor.Stopped, supervisor.Crashed:
					return false, nil
				}
				return true, a.Stop(ctx)
			},
			Start: func(ctx context.Context, name string) error { return dep.StartReserved(ctx, name) },
			Owner: func(name string) (int, int, error) {
				u, err := users.Lookup(name)
				return int(u.UID), int(u.GID), err
			},
		},
	}
}

// waitUploads gives background backup uploads a moment to finish.
func waitUploads(log *slog.Logger, bk *backup.Service, max time.Duration) {
	done := make(chan struct{})
	go func() { bk.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(max):
		log.Warn("backup uploads still running at shutdown; they will be retried after the restart")
	}
}

// newEdge creates the edge manager and its router (not started yet).
func newEdge(ctx context.Context, log *slog.Logger, cfg *config.Config, st *store.Store, box *secrets.Box,
	sup *supervisor.Supervisor, dep *deployer.Deployer) (*edge.Manager, error) {
	m, err := edge.NewManager(ctx, edge.Config{
		Listen: cfg.Edge.Listen, DashboardHost: cfg.Edge.DashboardDomain,
		PublicIPv4: cfg.Edge.PublicIPv4, PublicIPv6: cfg.Edge.PublicIPv6,
		AOP: cfg.Edge.AOPEnabled(), APIBase: cfg.Edge.CloudflareAPI, DataRoot: cfg.DataRoot,
	}, st, box, log)
	if err != nil {
		return nil, err
	}
	m.Router = &edge.Router{
		Dashboard: edge.PlaceholderDashboard(), DashboardHost: cfg.Edge.DashboardDomain, Log: log,
		State: func(name string) edge.Availability {
			if dep.Deploying(name) {
				return edge.Deploying
			}
			a := sup.Get(name)
			if a == nil {
				return edge.NotRunning
			}
			switch a.Status().State {
			case supervisor.Running:
				return edge.Available
			case supervisor.Starting:
				return edge.Starting
			}
			return edge.NotRunning
		},
	}
	return m, nil
}

func register(ctx context.Context, log *slog.Logger, sup *supervisor.Supervisor, dep *deployer.Deployer, la config.LoadedApp) error {
	if !la.Deployable() {
		if _, err := sup.Add(la.Spec); err != nil {
			return err
		}
		log.Info("prebuilt app registered", "app", la.Spec.Name, "port", la.Spec.Port, "release_dir", la.Spec.ReleaseDir)
		return nil
	}
	a, err := dep.Register(ctx, deployer.AppConfig{
		Base: la.Spec, Repo: la.Repo, Branch: la.Branch, Subdir: la.Subdir,
		BuildMemory: la.BuildMemory, BuildTimeout: la.BuildTimeout,
	})
	if err != nil {
		return err
	}
	rel := a.Spec().ReleaseID
	if rel == "" {
		rel = "none (deploy with: dootd ctl deploy " + la.Spec.Name + ")"
	}
	log.Info("app registered", "app", la.Spec.Name, "port", la.Spec.Port, "repo", la.Repo.URL, "branch", la.Branch, "release", rel)
	return nil
}

// logStatus prints every app's state (send SIGUSR1 to dootd).
func logStatus(log *slog.Logger, sup *supervisor.Supervisor) {
	for _, a := range sup.Apps() {
		s := a.Status()
		attrs := []any{"app", a.Spec().Name, "state", s.State, "pid", s.PID, "release", s.Release,
			"restarts", s.Restarts, "oom_kills", s.OOMKills}
		if s.LastExit != "" {
			attrs = append(attrs, "last_exit", s.LastExit)
		}
		if s.LastError != "" {
			attrs = append(attrs, "last_error", s.LastError)
		}
		if st, err := a.Group().Stats(); err == nil {
			attrs = append(attrs, "mem_bytes", st.MemoryCurrent, "pids", st.PidsCurrent, "cpu_usec", st.CPUUsageUsec)
		}
		log.Info("status", attrs...)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
