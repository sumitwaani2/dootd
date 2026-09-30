package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

	"github.com/sumitwaani2/dootd/internal/apps"
	"github.com/sumitwaani2/dootd/internal/auth"
	"github.com/sumitwaani2/dootd/internal/backup"
	"github.com/sumitwaani2/dootd/internal/builder"
	"github.com/sumitwaani2/dootd/internal/buildinfo"
	"github.com/sumitwaani2/dootd/internal/cgroup"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/metrics"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
	"github.com/sumitwaani2/dootd/internal/testenv"
	"github.com/sumitwaani2/dootd/internal/toolchain"
	"github.com/sumitwaani2/dootd/internal/users"
	"github.com/sumitwaani2/dootd/internal/web"
)

func runServe(stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := serve(log); err != nil {
		log.Error("dootd stopped with an error", "err", err)
		return 1
	}
	return 0
}

func serve(log *slog.Logger) error {
	if os.Geteuid() != 0 {
		return errors.New("dootd serve must run as root (it manages users and cgroups)")
	}
	log.Info("starting", "version", buildinfo.String())
	tuneMemory()

	lay := layout.Default()
	if err := layout.EnsureBase(lay); err != nil {
		return err
	}
	box, err := secrets.LoadOrCreate(layout.MasterKey)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, lay.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := os.Chmod(lay.DBPath(), 0o600); err != nil {
		return err
	}
	ver, _ := st.SchemaVersion(ctx)
	log.Info("state database ready", "path", lay.DBPath(), "schema", ver)

	cg, err := cgroup.Setup("")
	if err != nil {
		return err
	}
	log.Info("cgroup tree ready", "root", cg.Root(), "controllers", keys(cg.Controllers()))
	if cleaned, err := cg.ReapStale(ctx); err != nil {
		return err
	} else if len(cleaned) > 0 {
		log.Warn("killed leftover processes from a previous run", "groups", cleaned)
	}

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

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	authSvc := auth.New(st)
	edgeMgr, err := newEdge(ctx, log, lay, st, box, sup, dep, authSvc)
	if err != nil {
		return err
	}

	bk := newBackups(lay, st, box, sup, dep, log)
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

	appSvc := apps.New(st, box, dep, sup, edgeMgr, edgeMgr.DashboardHost, log)
	appSvc.SetBackups(bk)
	if err := appSvc.Load(ctx); err != nil {
		return err
	}
	for _, rt := range appSvc.Routes() {
		if dh := edgeMgr.DashboardHost(); dh != "" && rt.Host == dh {
			log.Error("an app uses the dashboard domain; it is not routed", "app", rt.App, "domain", rt.Host)
		}
	}

	mc := &metrics.Collector{Store: st, Sup: sup, DataRoot: lay.Root, Log: log.With("component", "metrics"),
		Requests: edgeMgr.Router.Stats}

	dash := &web.Server{
		Auth: authSvc, Apps: appSvc, Dep: dep, Sup: sup, Edge: edgeMgr, Zig: dep.Zig, Store: st,
		Backups: bk, Metrics: mc,
		Thresholds: web.Thresholds{DiskPercent: testenv.WarnPercent(85), MemoryPercent: testenv.WarnPercent(90), CertDays: 14},
		Layout:     lay, Version: buildinfo.Version, Log: log.With("component", "web"),
	}
	h, err := dash.Handler()
	if err != nil {
		return err
	}
	edgeMgr.Router.Dashboard = h
	if edgeMgr.DashboardHost() == "" {
		log.Warn("no dashboard domain yet: the dashboard is only on the setup address (https://<server IP>)")
	}
	appSvc.RefreshEdge()
	edgeErr := make(chan error, 1)
	go func() { edgeErr <- edgeMgr.Serve(runCtx) }()
	edgeMgr.Run(runCtx)

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
		case <-runCtx.Done():
			break loop
		}
	}

	log.Info("shutting down: cancelling deployments")
	dep.Stop()
	waitUploads(log, bk, 20*time.Second)
	mc.Wait()
	log.Info("stopping apps")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.StopAll(stopCtx); err != nil {
		return fmt.Errorf("stopping apps: %w", err)
	}
	log.Info("all apps stopped; bye")
	return nil
}

// newBackups creates the backup service and connects it to the apps.
func newBackups(lay layout.Layout, st *store.Store, box *secrets.Box, sup *supervisor.Supervisor, dep *deployer.Deployer, log *slog.Logger) *backup.Service {
	return &backup.Service{
		Layout: lay, Store: st, Box: box, Log: log.With("component", "backup"),
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

// newEdge creates the edge manager and its router (not started yet). The
// public IPs and API base are only overridden by the E2E tests.
func newEdge(ctx context.Context, log *slog.Logger, lay layout.Layout, st *store.Store, box *secrets.Box,
	sup *supervisor.Supervisor, dep *deployer.Deployer, au *auth.Auth) (*edge.Manager, error) {
	m, err := edge.NewManager(ctx, edge.Config{
		Listen: ":443", PublicIPv4: testenv.PublicIPv4(), PublicIPv6: testenv.PublicIPv6(),
		AOP: true, APIBase: testenv.CloudflareAPI(), DataRoot: lay.Root,
		SetupPending: func() bool { return !au.SetupUntil().IsZero() },
	}, st, box, log)
	if err != nil {
		return nil, err
	}
	m.Router = &edge.Router{
		Log: log,
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
	m.Router.SetDashboardHost(m.DashboardHost())
	return m, nil
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

// tuneMemory keeps dootd inside its 30 MB resident budget (Req 1.3): a
// soft heap limit makes the GC run more often before the heap grows, and
// freed memory is handed back to the kernel every 2 minutes instead of
// being kept for reuse. GOMEMLIMIT / GOGC in the environment override.
func tuneMemory() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(12 << 20)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
	go func() {
		for range time.Tick(2 * time.Minute) {
			debug.FreeOSMemory()
		}
	}()
}
