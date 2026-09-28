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

	"github.com/sumitwaani2/dootd/internal/builder"
	"github.com/sumitwaani2/dootd/internal/buildinfo"
	"github.com/sumitwaani2/dootd/internal/cgroup"
	"github.com/sumitwaani2/dootd/internal/config"
	"github.com/sumitwaani2/dootd/internal/control"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
	"github.com/sumitwaani2/dootd/internal/toolchain"
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

	if devApps != "" {
		apps, err := config.LoadDevApps(devApps)
		if err != nil {
			return err
		}
		for _, la := range apps {
			if err := register(ctx, sup, dep, la); err != nil {
				return err
			}
		}
	} else {
		log.Info("no apps configured (use --dev-apps until the dashboard exists)")
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ctl := &control.Server{Sup: sup, Dep: dep, Layout: lay, Log: log}
	ctlErr := make(chan error, 1)
	go func() { ctlErr <- ctl.Serve(runCtx, socket) }()
	log.Info("control socket ready", "path", socket)

	dep.Run(runCtx)
	go sup.StartAll(runCtx, func(name string) bool { return dep.DesiredRunning(runCtx, name) })

	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)

loop:
	for {
		select {
		case <-usr1:
			logStatus(log, sup)
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

func register(ctx context.Context, sup *supervisor.Supervisor, dep *deployer.Deployer, la config.LoadedApp) error {
	if !la.Deployable() {
		if _, err := sup.Add(la.Spec); err != nil {
			return err
		}
		slog.Info("prebuilt app registered", "app", la.Spec.Name, "port", la.Spec.Port, "release_dir", la.Spec.ReleaseDir)
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
	slog.Info("app registered", "app", la.Spec.Name, "port", la.Spec.Port, "repo", la.Repo.URL, "branch", la.Branch, "release", rel)
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
