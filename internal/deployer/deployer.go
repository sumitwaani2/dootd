// Package deployer implements the deploy pipeline (docs/architecture.md
// §11): clone → validate dootd.toml → ensure Zig → build → release →
// stop old → pre-deploy backup → switch → start + health check, with
// automatic rollback when the new release is unhealthy.
//
// All deploys and rollbacks on the host run one at a time from a single
// queue, so builds never compete for a small VPS's memory.
package deployer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sumitwaani2/dootd/internal/app"
	"github.com/sumitwaani2/dootd/internal/builder"
	"github.com/sumitwaani2/dootd/internal/github"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/manifest"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
	"github.com/sumitwaani2/dootd/internal/toolchain"
	"github.com/sumitwaani2/dootd/internal/users"
)

// Retention.
const (
	KeepReleases  = 3
	KeepBuildLogs = 20
)

// Settings keys.
const (
	SettingGitHubToken = "github_token"
	purposeGitHubToken = "settings:github_token"
)

// AppConfig is the deploy configuration of one app.
type AppConfig struct {
	Base         app.Spec // name, type, domain, port, env, limits (no release fields)
	Repo         github.Repo
	Branch       string
	Subdir       string
	BuildMemory  int64
	BuildTimeout time.Duration
}

// Deps are the deployer's collaborators.
type Deps struct {
	Layout     layout.Layout
	Store      *store.Store
	Secrets    *secrets.Box
	Supervisor *supervisor.Supervisor
	Builder    *builder.Builder
	Zig        *toolchain.Zig
	Log        *slog.Logger
	// PreDeploy runs after the old release stopped and before the switch.
	// Phase 5 plugs the SQLite backup in here. A returned error aborts the
	// deploy and restarts the old release.
	PreDeploy func(ctx context.Context, app string, log *logs.Log) error
}

// Deployer runs deployments.
type Deployer struct {
	Deps
	db  *store.Store
	log *slog.Logger

	mu        sync.Mutex
	apps      map[string]AppConfig
	pending   map[string]int64  // app -> queued or running deployment
	held      map[string]string // app -> reason (e.g. a restore) that blocks deployments
	deploying map[string]bool   // app is between "stop old" and "healthy/failed"

	queue  chan job
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type job struct {
	id      int64
	app     string
	kind    string
	release string // rollback target
}

// New creates a deployer, marks interrupted deployments as failed and
// cleans leftover build workspaces. Call Run to start the worker.
func New(ctx context.Context, d Deps) (*Deployer, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	dp := &Deployer{
		Deps:      d,
		db:        d.Store,
		log:       d.Log,
		apps:      map[string]AppConfig{},
		pending:   map[string]int64{},
		held:      map[string]string{},
		deploying: map[string]bool{},
		queue:     make(chan job, 64),
	}
	if n, err := dp.recoverInterrupted(ctx); err != nil {
		return nil, err
	} else if n > 0 {
		dp.log.Warn("marked deployments interrupted by a restart as failed", "count", n)
	}
	d.Zig.CleanTemp()
	return dp, nil
}

// Register adds an app and its current release (if any) to the supervisor.
func (d *Deployer) Register(ctx context.Context, cfg AppConfig) (*supervisor.App, error) {
	spec := cfg.Base
	if cur := d.currentRelease(spec.Name); cur != "" {
		rel, err := d.release(ctx, spec.Name, cur)
		if err != nil {
			d.log.Warn("current release has no record; app needs a deploy", "app", spec.Name, "release", cur, "err", err)
		} else {
			spec = d.specFor(cfg, rel)
		}
	}
	a, err := d.Supervisor.Add(spec)
	if err != nil {
		return nil, err
	}
	for _, e := range []string{d.Layout.BuildsDir(spec.Name)} {
		entries, _ := os.ReadDir(e)
		for _, x := range entries {
			os.RemoveAll(filepath.Join(e, x.Name()))
		}
	}
	d.mu.Lock()
	d.apps[spec.Name] = cfg
	d.mu.Unlock()
	return a, nil
}

func (d *Deployer) specFor(cfg AppConfig, r Release) app.Spec {
	s := cfg.Base
	s.ReleaseID = r.ID
	s.ReleaseDir = filepath.Join(d.Layout.ReleaseDir(s.Name, r.ID), r.Subdir)
	s.Run = r.Run
	s.HealthPath = r.HealthPath
	return s
}

// currentRelease reads the `current` symlink ("" if none).
func (d *Deployer) currentRelease(appName string) string {
	t, err := os.Readlink(d.Layout.CurrentLink(appName))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// Deploying reports whether app is in the downtime window of a deploy
// (the router shows a 503 "deploying" page then; Phase 3).
func (d *Deployer) Deploying(appName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deploying[appName]
}

// Deploy queues a build + deploy of the branch HEAD.
func (d *Deployer) Deploy(ctx context.Context, appName string) (int64, error) {
	return d.enqueue(ctx, appName, KindDeploy, "")
}

// Rollback queues a switch to a kept release (no build).
func (d *Deployer) Rollback(ctx context.Context, appName, releaseID string) (int64, error) {
	if _, err := d.release(ctx, appName, releaseID); err != nil {
		return 0, err
	}
	if d.currentRelease(appName) == releaseID {
		return 0, fmt.Errorf("release %s is already current", releaseID)
	}
	return d.enqueue(ctx, appName, KindRollback, releaseID)
}

func (d *Deployer) enqueue(ctx context.Context, appName, kind, release string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.apps[appName]; !ok {
		return 0, fmt.Errorf("unknown app %q", appName)
	}
	if id, ok := d.pending[appName]; ok {
		return 0, fmt.Errorf("deployment #%d of %s is still in progress", id, appName)
	}
	if why, ok := d.held[appName]; ok {
		return 0, fmt.Errorf("%s: %s is in progress; deploy when it has finished", appName, why)
	}
	if d.ctx == nil || d.ctx.Err() != nil {
		return 0, errors.New("deployer is not running")
	}
	id, err := d.insertDeployment(ctx, appName, kind, release)
	if err != nil {
		return 0, err
	}
	select {
	case d.queue <- job{id: id, app: appName, kind: kind, release: release}:
	default:
		d.finish(id, errors.New("deploy queue is full"))
		return 0, errors.New("deploy queue is full")
	}
	d.pending[appName] = id
	return id, nil
}

// Run starts the worker. Stop cancels the running job and waits.
func (d *Deployer) Run(ctx context.Context) {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.ctx.Done():
				for {
					select {
					case j := <-d.queue:
						d.finish(j.id, errors.New("cancelled: dootd is shutting down"))
					default:
						return
					}
				}
			case j := <-d.queue:
				d.runJob(j)
			}
		}
	}()
}

// Stop cancels the current job and waits for the worker to exit.
func (d *Deployer) Stop() {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	d.wg.Wait()
}

func (d *Deployer) runJob(j job) {
	d.mu.Lock()
	cfg := d.apps[j.app]
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, j.app)
		delete(d.deploying, j.app)
		d.mu.Unlock()
	}()

	lg, err := logs.Open(d.Layout.BuildLog(j.app, j.id), logs.Options{MaxSize: 256 << 20, Keep: 1, RingSize: 16})
	if err != nil {
		d.finish(j.id, err)
		return
	}
	start := time.Now()
	d.setStatus(j.id, StatusBuilding)
	d.log.Info("deployment started", "app", j.app, "id", j.id, "kind", j.kind)

	if j.kind == KindRollback {
		lg.Writef("rollback of %s to release %s", j.app, j.release)
		err = d.rollback(d.ctx, cfg, j, lg)
	} else {
		lg.Writef("deploy of %s from %s (branch %s)", j.app, displayRepo(cfg.Repo), cfg.Branch)
		err = d.deploy(d.ctx, cfg, j, lg)
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	if err != nil {
		lg.Writef("FAILED after %s: %v", took, err)
		d.log.Error("deployment failed", "app", j.app, "id", j.id, "err", err)
	} else {
		lg.Writef("SUCCEEDED in %s", took)
		d.log.Info("deployment succeeded", "app", j.app, "id", j.id, "took", took)
	}
	lg.Close()
	d.finish(j.id, err)
	d.pruneBuildLogs(j.app)
}

func displayRepo(r github.Repo) string {
	if r.GitHub {
		return r.Owner + "/" + r.Name
	}
	return r.URL
}

// GitHubToken returns the stored GitHub token ("" if none).
func (d *Deployer) GitHubToken(ctx context.Context) (string, error) {
	v, ok, err := d.Store.GetSetting(ctx, SettingGitHubToken)
	if err != nil || !ok {
		return "", err
	}
	b, err := d.Secrets.Open(v, purposeGitHubToken)
	if err != nil {
		return "", fmt.Errorf("stored GitHub token: %w", err)
	}
	return string(b), nil
}

// SetGitHubToken stores the account-level GitHub token encrypted
// (empty removes it).
func (d *Deployer) SetGitHubToken(ctx context.Context, token string) error {
	if token == "" {
		_, err := d.Store.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingGitHubToken)
		return err
	}
	sealed, err := d.Secrets.Seal([]byte(token), purposeGitHubToken)
	if err != nil {
		return err
	}
	return d.Store.SetSetting(ctx, SettingGitHubToken, sealed)
}

func (d *Deployer) deploy(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) error {
	name := cfg.Base.Name
	u, err := users.Lookup(name)
	if err != nil {
		return err
	}
	ws := filepath.Join(d.Layout.BuildsDir(name), strconv.FormatInt(j.id, 10))
	os.RemoveAll(ws)
	defer os.RemoveAll(ws) // no-op once moved into releases/

	// 1. Clone. The running release keeps serving during steps 1-5.
	var token string
	if cfg.Repo.GitHub {
		if token, err = d.GitHubToken(ctx); err != nil {
			return err
		}
	}
	lg.Writef("cloning (depth 1)")
	co, err := github.Clone(ctx, cfg.Repo, cfg.Branch, token, ws)
	if err != nil {
		return err
	}
	releaseID := time.Now().UTC().Format("20060102-150405") + "-" + co.SHA[:7]
	d.setRelease(j.id, releaseID, co.SHA)
	lg.Writef("commit %s %q -> release %s", co.SHA[:12], co.Subject, releaseID)

	// 2. dootd.toml.
	appRoot := filepath.Join(ws, cfg.Subdir)
	m, err := manifest.Load(appRoot, cfg.Base.Type)
	if err != nil {
		return err
	}
	lg.Writef("dootd.toml: zig %s, build %q, run %q, health %s", m.ZigVersion, m.Build, strings.Join(m.Run, " "), m.HealthPath)

	// 3. Pinned toolchain.
	zigDir, err := d.Zig.Ensure(ctx, m.ZigVersion, func(s string) { lg.Writef("%s", s) })
	if err != nil {
		return err
	}

	// 4. Build as the app user in its build cgroup.
	err = d.Builder.Run(ctx, builder.Job{
		App: name, User: u, Workspace: ws, Subdir: cfg.Subdir, Command: m.Build, ZigDir: zigDir,
		Env: cfg.Base.Env, MemoryMax: cfg.BuildMemory, Timeout: cfg.BuildTimeout, Log: lg,
	})
	if err != nil {
		return err
	}
	if err := builder.Finalize(ws, u); err != nil {
		return fmt.Errorf("finalize release: %w", err)
	}
	if err := builder.CheckBinary(appRoot, m.Run[0]); err != nil {
		return err
	}

	// 5. Keep it as an immutable release.
	relDir := d.Layout.ReleaseDir(name, releaseID)
	if err := os.Rename(ws, relDir); err != nil {
		return fmt.Errorf("store release: %w", err)
	}
	if err := os.Chmod(relDir, 0o755); err != nil {
		return err
	}
	rel := Release{
		App: name, ID: releaseID, GitSHA: co.SHA, Subject: co.Subject, Branch: cfg.Branch, Subdir: cfg.Subdir,
		ZigVersion: m.ZigVersion, Run: m.Run, HealthPath: m.HealthPath, CreatedAt: time.Now(),
	}
	if err := d.insertRelease(ctx, rel); err != nil {
		os.RemoveAll(relDir)
		return fmt.Errorf("record release: %w", err)
	}

	// 6-8. Switch with health check; roll back on failure.
	if err := d.activate(ctx, cfg, rel, j.id, lg); err != nil {
		d.removeRelease(name, releaseID, lg)
		return err
	}
	d.pruneReleases(name, lg)
	return nil
}

func (d *Deployer) rollback(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) error {
	rel, err := d.release(ctx, cfg.Base.Name, j.release)
	if err != nil {
		return err
	}
	d.setRelease(j.id, rel.ID, rel.GitSHA)
	return d.activate(ctx, cfg, rel, j.id, lg)
}

// activate stops the old release, runs the pre-deploy hook, switches the
// `current` symlink and starts rel. If rel does not become healthy, the
// previous release is restored.
func (d *Deployer) activate(ctx context.Context, cfg AppConfig, rel Release, id int64, lg *logs.Log) error {
	name := cfg.Base.Name
	a := d.Supervisor.Get(name)
	if a == nil {
		return fmt.Errorf("app %s is not registered", name)
	}
	u, err := users.Lookup(name)
	if err != nil {
		return err
	}
	prevSpec := a.Spec()
	prevState := a.Status().State
	wasRunning := prevState == supervisor.Running || prevState == supervisor.Starting || prevState == supervisor.Backoff
	d.setStatus(id, StatusDeploying)

	d.mu.Lock()
	d.deploying[name] = true
	d.mu.Unlock()
	downStart := time.Now()
	defer func() {
		d.mu.Lock()
		delete(d.deploying, name)
		d.mu.Unlock()
	}()

	// Detached from the job context so a shutdown mid-switch still leaves
	// a consistent state.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	if prevSpec.ReleaseID != "" {
		lg.Writef("stopping release %s (state %s)", prevSpec.ReleaseID, prevState)
	}
	if err := a.Stop(sctx); err != nil {
		return fmt.Errorf("stop old release: %w", err)
	}

	restorePrev := func(reason error) error {
		if prevSpec.ReleaseID == "" {
			os.Remove(d.Layout.CurrentLink(name))
			a.Update(sctx, cfg.Base)
			lg.Writef("no previous release to go back to; the app stays stopped")
			return reason
		}
		if err := d.swapCurrent(name, prevSpec.ReleaseID); err != nil {
			return fmt.Errorf("%v; restoring the previous release also failed: %w", reason, err)
		}
		if err := a.Update(sctx, prevSpec); err != nil {
			return fmt.Errorf("%v; restoring the previous release also failed: %w", reason, err)
		}
		if !wasRunning {
			lg.Writef("switched back to release %s (left stopped, as it was before)", prevSpec.ReleaseID)
			return fmt.Errorf("%w; switched back to release %s", reason, prevSpec.ReleaseID)
		}
		if err := a.Start(sctx); err != nil {
			lg.Writef("previous release %s also failed to start: %v", prevSpec.ReleaseID, err)
			return fmt.Errorf("%w; rolled back to release %s, which also failed to start: %v", reason, prevSpec.ReleaseID, err)
		}
		lg.Writef("rolled back: release %s is serving again", prevSpec.ReleaseID)
		return fmt.Errorf("%w; rolled back to release %s", reason, prevSpec.ReleaseID)
	}

	if d.PreDeploy != nil {
		if err := d.PreDeploy(sctx, name, lg); err != nil {
			return restorePrev(fmt.Errorf("pre-deploy backup failed: %w", err))
		}
	}
	if err := d.swapCurrent(name, rel.ID); err != nil {
		return restorePrev(fmt.Errorf("switch release: %w", err))
	}
	if err := d.Layout.WipeTmp(name, u); err != nil {
		lg.Writef("warning: %v", err)
	}
	newSpec := d.specFor(cfg, rel)
	if err := a.Update(sctx, newSpec); err != nil {
		return restorePrev(err)
	}
	lg.Writef("starting release %s", rel.ID)
	if err := a.Start(sctx); err != nil {
		lg.Writef("release %s is not healthy: %v", rel.ID, err)
		if e := a.Stop(sctx); e != nil {
			lg.Writef("warning: stopping the failed release: %v", e)
		}
		return restorePrev(fmt.Errorf("new release failed its health check: %w", err))
	}
	lg.Writef("release %s is healthy; downtime %s", rel.ID, time.Since(downStart).Round(10*time.Millisecond))
	if err := d.SetDesired(sctx, name, true); err != nil {
		lg.Writef("warning: %v", err)
	}
	return nil
}

// swapCurrent atomically points apps/<app>/current at releases/<id>.
func (d *Deployer) swapCurrent(appName, releaseID string) error {
	link := d.Layout.CurrentLink(appName)
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("releases", releaseID), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (d *Deployer) removeRelease(appName, id string, lg *logs.Log) {
	if d.currentRelease(appName) == id {
		return
	}
	if err := os.RemoveAll(d.Layout.ReleaseDir(appName, id)); err != nil {
		lg.Writef("warning: removing release %s: %v", id, err)
	}
	if err := d.deleteRelease(appName, id); err != nil {
		lg.Writef("warning: forgetting release %s: %v", id, err)
	}
}

// pruneReleases keeps the current release plus the newest others, up to
// KeepReleases in total, and deletes stray release directories.
func (d *Deployer) pruneReleases(appName string, lg *logs.Log) {
	rs, err := d.Releases(context.Background(), appName)
	if err != nil {
		lg.Writef("warning: listing releases: %v", err)
		return
	}
	keep := map[string]bool{}
	for _, r := range rs {
		if r.Current {
			keep[r.ID] = true
		}
	}
	for _, r := range rs {
		if len(keep) >= KeepReleases {
			break
		}
		keep[r.ID] = true
	}
	for _, r := range rs {
		if !keep[r.ID] {
			lg.Writef("pruning old release %s", r.ID)
			d.removeRelease(appName, r.ID, lg)
		}
	}
	entries, _ := os.ReadDir(d.Layout.ReleasesDir(appName))
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(d.Layout.ReleasesDir(appName), e.Name()))
		}
	}
}

func (d *Deployer) pruneBuildLogs(appName string) {
	dir := d.Layout.BuildLogsDir(appName)
	entries, _ := os.ReadDir(dir)
	var ids []int64
	for _, e := range entries {
		base := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".1"), ".log")
		if n, err := strconv.ParseInt(base, 10, 64); err == nil && !containsID(ids, n) {
			ids = append(ids, n)
		}
	}
	sort.Slice(ids, func(i, k int) bool { return ids[i] > ids[k] })
	for _, id := range ids[min(len(ids), KeepBuildLogs):] {
		os.Remove(d.Layout.BuildLog(appName, id))
		os.Remove(d.Layout.BuildLog(appName, id) + ".1")
	}
}

func containsID(ids []int64, n int64) bool {
	for _, x := range ids {
		if x == n {
			return true
		}
	}
	return false
}

// Apps returns the registered app names (sorted).
func (d *Deployer) Apps() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.apps))
	for n := range d.apps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Config returns an app's deploy configuration.
func (d *Deployer) Config(appName string) (AppConfig, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.apps[appName]
	return c, ok
}

// Pending returns the queued or running deployment of app (0 if none).
func (d *Deployer) Pending(appName string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending[appName]
}
