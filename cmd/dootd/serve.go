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
	"syscall"
	"time"

	"github.com/sumitwaani2/dootd/internal/buildinfo"
	"github.com/sumitwaani2/dootd/internal/cgroup"
	"github.com/sumitwaani2/dootd/internal/config"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
)

func runServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", config.DefaultPath, "path to config.toml")
	devApps := fs.String("dev-apps", "", "TEMPORARY (until the dashboard exists): TOML file with prebuilt apps to run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfgExplicit := false
	fs.Visit(func(f *flag.Flag) { cfgExplicit = cfgExplicit || f.Name == "config" })

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := serve(log, *cfgPath, cfgExplicit, *devApps); err != nil {
		log.Error("dootd stopped with an error", "err", err)
		return 1
	}
	return 0
}

func serve(log *slog.Logger, cfgPath string, cfgExplicit bool, devApps string) error {
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
	if _, err := secrets.LoadOrCreate(cfg.MasterKey); err != nil {
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

	sup := supervisor.New(supervisor.Deps{Layout: layout.Layout{Root: cfg.DataRoot}, Cgroups: cg, Log: log})

	if devApps != "" {
		specs, err := config.LoadDevApps(devApps)
		if err != nil {
			return err
		}
		for _, s := range specs {
			if _, err := sup.Add(s); err != nil {
				return err
			}
			log.Info("app registered", "app", s.Name, "port", s.Port, "release_dir", s.ReleaseDir)
		}
	} else {
		log.Info("no apps configured (use --dev-apps until the dashboard exists)")
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)

	go sup.StartAll(runCtx)

	for {
		select {
		case <-usr1:
			logStatus(log, sup)
			continue
		case <-runCtx.Done():
		}
		break
	}

	log.Info("shutting down: stopping apps")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.StopAll(stopCtx); err != nil {
		return fmt.Errorf("stopping apps: %w", err)
	}
	log.Info("all apps stopped; bye")
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
	return out
}
