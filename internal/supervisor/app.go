package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sumitwaani2/dootd/internal/app"
	"github.com/sumitwaani2/dootd/internal/cgroup"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/users"
)

// State of an app.
type State string

// States.
const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Backoff  State = "backoff" // waiting to restart after a failure
	Crashed  State = "crashed" // too many failures; needs a manual start
)

// Policy holds the timing rules (docs/architecture.md).
type Policy struct {
	HealthTimeout  time.Duration // max time from spawn to healthy
	StopGrace      time.Duration // SIGTERM → SIGKILL
	BackoffMin     time.Duration
	BackoffMax     time.Duration
	CrashWindow    time.Duration
	CrashLimit     int           // exits within CrashWindow that mark the app crashed
	StableAfter    time.Duration // healthy this long resets the backoff
	OOMPoll        time.Duration
	NoFile         uint64
	HealthInterval time.Duration
}

// DefaultPolicy matches the documented behaviour.
var DefaultPolicy = Policy{
	HealthTimeout:  30 * time.Second,
	StopGrace:      10 * time.Second,
	BackoffMin:     time.Second,
	BackoffMax:     60 * time.Second,
	CrashWindow:    5 * time.Minute,
	CrashLimit:     5,
	StableAfter:    60 * time.Second,
	OOMPoll:        10 * time.Second,
	NoFile:         app.DefaultNoFile,
	HealthInterval: 200 * time.Millisecond,
}

// Status is a snapshot of an app's runtime state.
type Status struct {
	State      State
	PID        int
	Release    string
	StartedAt  time.Time // current process spawn time
	HealthyAt  time.Time
	Restarts   int // automatic restarts since the last manual start
	LastExit   string
	LastExitAt time.Time
	LastError  string
	OOMKills   int64
	NextStart  time.Time // when in Backoff
}

// ErrStopped is returned to Start waiters when the app is stopped meanwhile.
var ErrStopped = errors.New("app was stopped")

// ErrNoRelease is returned by Start for an app that was never deployed.
var ErrNoRelease = errors.New("app has no release yet; deploy it first")

type op int

const (
	opStart op = iota
	opStop
	opUpdate
)

type command struct {
	op    op
	spec  app.Spec
	reply chan error
}

type exitEvent struct {
	gen   int
	state *os.ProcessState
	err   error
}

type healthEvent struct {
	gen int
	err error
}

// App is one supervised app. All mutable process state is owned by the
// loop goroutine; Status() reads a mutex-protected snapshot.
type App struct {
	dirs   layout.Layout
	user   users.User
	group  *cgroup.Group
	log    *logs.Log
	policy Policy
	slog   *slog.Logger

	cmds     chan command
	exitCh   chan exitEvent
	healthCh chan healthEvent
	quit     chan struct{}
	quitOnce sync.Once

	mu     sync.RWMutex
	spec   app.Spec
	status Status

	// loop-owned
	gen          int
	cmd          *exec.Cmd
	desired      State // Running or Stopped
	startWaiters []chan error
	stopWaiters  []chan error
	pendingErr   error // why a starting process is being killed
	exits        []time.Time
	consecutive  int
	lastOOM      int64
	backoff      *time.Timer
	stopTimer    *time.Timer
	healthCancel context.CancelFunc
}

func newApp(spec app.Spec, dirs layout.Layout, u users.User, g *cgroup.Group, lg *logs.Log, p Policy, l *slog.Logger) *App {
	a := &App{
		dirs: dirs, user: u, group: g, log: lg, policy: p, slog: l,
		cmds:     make(chan command),
		exitCh:   make(chan exitEvent, 1),
		healthCh: make(chan healthEvent, 1),
		quit:     make(chan struct{}),
		spec:     spec,
		status:   Status{State: Stopped, Release: spec.ReleaseID},
		desired:  Stopped,
	}
	if st, err := g.Stats(); err == nil {
		a.lastOOM = st.OOMKills
	}
	return a
}

// Spec returns the app's current spec.
func (a *App) Spec() app.Spec {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.spec
}

// Status returns a snapshot of the app's state.
func (a *App) Status() Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}

// Log returns the app's log (for live tailing).
func (a *App) Log() *logs.Log { return a.log }

// Group returns the app's cgroup (for metrics).
func (a *App) Group() *cgroup.Group { return a.group }

// Start starts the app and waits until it is healthy or has failed.
func (a *App) Start(ctx context.Context) error { return a.do(ctx, command{op: opStart}) }

// Stop stops the app (SIGTERM, grace period, then kill the whole cgroup).
func (a *App) Stop(ctx context.Context) error { return a.do(ctx, command{op: opStop}) }

// Restart stops then starts the app.
func (a *App) Restart(ctx context.Context) error {
	if err := a.Stop(ctx); err != nil {
		return err
	}
	return a.Start(ctx)
}

// Update replaces the spec (e.g. a new release). The app must be stopped or
// crashed. Name and user cannot change.
func (a *App) Update(ctx context.Context, spec app.Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	return a.do(ctx, command{op: opUpdate, spec: spec})
}

func (a *App) do(ctx context.Context, c command) error {
	c.reply = make(chan error, 1)
	select {
	case a.cmds <- c:
	case <-a.quit:
		return errors.New("supervisor: app actor is shut down")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) close() {
	a.quitOnce.Do(func() { close(a.quit) })
	a.log.Close()
}

func (a *App) setStatus(f func(*Status)) {
	a.mu.Lock()
	f(&a.status)
	a.mu.Unlock()
}

func (a *App) state() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status.State
}

func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func (a *App) loop() {
	oom := time.NewTicker(a.policy.OOMPoll)
	defer oom.Stop()
	for {
		select {
		case <-a.quit:
			return
		case c := <-a.cmds:
			switch c.op {
			case opStart:
				a.handleStart(c.reply)
			case opStop:
				a.handleStop(c.reply)
			case opUpdate:
				a.handleUpdate(c)
			}
		case ev := <-a.exitCh:
			a.handleExit(ev)
		case ev := <-a.healthCh:
			a.handleHealth(ev)
		case <-timerC(a.backoff):
			a.backoff = nil
			a.spawn()
		case <-timerC(a.stopTimer):
			a.stopTimer = nil
			a.log.Writef("did not exit within %s after SIGTERM; killing", a.policy.StopGrace)
			a.killGroup()
		case <-oom.C:
			a.checkOOM()
		}
	}
}

func (a *App) handleStart(reply chan error) {
	if a.Spec().ReleaseID == "" {
		reply <- ErrNoRelease
		return
	}
	a.desired = Running
	switch a.state() {
	case Running:
		reply <- nil
	case Starting:
		a.startWaiters = append(a.startWaiters, reply)
	case Stopping:
		reply <- errors.New("app is stopping; try again when it has stopped")
	default: // Stopped, Crashed, Backoff: a manual start resets the crash history
		if a.backoff != nil {
			a.backoff.Stop()
			a.backoff = nil
		}
		a.exits, a.consecutive = nil, 0
		a.setStatus(func(s *Status) { s.Restarts, s.LastError, s.NextStart = 0, "", time.Time{} })
		a.startWaiters = append(a.startWaiters, reply)
		a.spawn()
	}
}

func (a *App) handleStop(reply chan error) {
	a.desired = Stopped
	switch a.state() {
	case Stopped, Crashed, Backoff:
		if a.backoff != nil {
			a.backoff.Stop()
			a.backoff = nil
		}
		a.setStatus(func(s *Status) { s.State, s.NextStart = Stopped, time.Time{} })
		reply <- nil
	case Stopping:
		a.stopWaiters = append(a.stopWaiters, reply)
	case Starting, Running:
		a.stopWaiters = append(a.stopWaiters, reply)
		a.replyStart(ErrStopped)
		a.setStatus(func(s *Status) { s.State = Stopping })
		a.log.Writef("stopping (SIGTERM to pid %d)", a.cmd.Process.Pid)
		if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			a.slog.Warn("SIGTERM failed", "err", err)
		}
		a.stopTimer = time.NewTimer(a.policy.StopGrace)
	}
}

func (a *App) handleUpdate(c command) {
	st := a.state()
	if st != Stopped && st != Crashed {
		c.reply <- fmt.Errorf("app must be stopped to update it (state: %s)", st)
		return
	}
	cur := a.Spec()
	if c.spec.Name != cur.Name {
		c.reply <- errors.New("app name cannot change")
		return
	}
	if err := applyLimits(a.group, c.spec.Limits); err != nil {
		c.reply <- err
		return
	}
	a.mu.Lock()
	a.spec = c.spec
	a.status.Release = c.spec.ReleaseID
	a.mu.Unlock()
	c.reply <- nil
}

// spawn starts a new process and a health check for it.
func (a *App) spawn() {
	a.gen++
	a.pendingErr = nil
	spec := a.Spec()
	cmd, err := a.startProcess(spec)
	if err != nil {
		a.log.Writef("failed to start: %v", err)
		a.setStatus(func(s *Status) { s.LastError = err.Error() })
		a.failure(err)
		return
	}
	a.cmd = cmd
	now := time.Now()
	a.setStatus(func(s *Status) {
		s.State, s.PID, s.StartedAt, s.HealthyAt, s.NextStart = Starting, cmd.Process.Pid, now, time.Time{}, time.Time{}
		s.Release = spec.ReleaseID
	})
	a.log.Writef("started pid %d (release %s): %s", cmd.Process.Pid, spec.ReleaseID, strconv.Quote(joinArgs(spec.Run)))

	gen := a.gen
	go func() {
		err := cmd.Wait()
		ev := exitEvent{gen: gen, state: cmd.ProcessState, err: err}
		select {
		case a.exitCh <- ev:
		case <-a.quit:
		}
	}()
	hctx, hcancel := context.WithCancel(context.Background())
	a.healthCancel = hcancel
	go func() {
		defer hcancel()
		go func() {
			select {
			case <-a.quit:
				hcancel()
			case <-hctx.Done():
			}
		}()
		err := healthCheck(hctx, spec, a.policy)
		select {
		case a.healthCh <- healthEvent{gen: gen, err: err}:
		case <-a.quit:
		}
	}()
}

func (a *App) startProcess(spec app.Spec) (*exec.Cmd, error) {
	bin := spec.Run[0]
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(spec.ReleaseDir, bin)
	}
	st, err := os.Stat(bin)
	if err != nil {
		return nil, fmt.Errorf("run binary: %w", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o001 == 0 {
		return nil, fmt.Errorf("run binary %s must be a regular file executable by others (mode %s)", bin, st.Mode())
	}

	fd, err := a.group.OpenFD()
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}

	cmd := exec.Command(bin, spec.Run[1:]...)
	cmd.Dir = spec.ReleaseDir
	cmd.Env = a.environ(spec)
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &syscall.Credential{Uid: a.user.UID, Gid: a.user.GID, Groups: []uint32{}},
		Setpgid:     true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true, // clone3(CLONE_INTO_CGROUP): the child starts inside its cgroup
		CgroupFD:    fd,
	}
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, err
	}

	lim := &unix.Rlimit{Cur: a.policy.NoFile, Max: a.policy.NoFile}
	if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_NOFILE, lim, nil); err != nil {
		a.slog.Warn("setting open-files limit failed", "err", err)
	}

	go func() { a.log.Capture(logs.Stdout, outR); outR.Close() }()
	go func() { a.log.Capture(logs.Stderr, errR); errR.Close() }()
	return cmd, nil
}

// environ builds the contract environment (docs/app-contract.md §3). The
// process does not inherit dootd's environment.
func (a *App) environ(spec app.Spec) []string {
	env := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin",
		"LANG": "C.UTF-8",
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	contract := map[string]string{
		"PORT":           strconv.Itoa(spec.Port),
		"HOST":           "127.0.0.1",
		"DATA_DIR":       a.dataDir(spec),
		"TMPDIR":         a.tmpDir(spec),
		"DOOTD_APP":      spec.Name,
		"DOOTD_DOMAIN":   spec.Domain,
		"DOOTD_RELEASE":  spec.ReleaseID,
		"DOOTD_CONTRACT": strconv.Itoa(app.ContractVersion),
	}
	for k, v := range contract {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (a *App) dataDir(spec app.Spec) string { return a.dirs.DataDir(spec.Name) }
func (a *App) tmpDir(spec app.Spec) string  { return a.dirs.TmpDir(spec.Name) }

func (a *App) handleHealth(ev healthEvent) {
	if ev.gen != a.gen || a.state() != Starting {
		return
	}
	if ev.err == nil {
		now := time.Now()
		a.setStatus(func(s *Status) { s.State, s.HealthyAt = Running, now })
		a.log.Writef("healthy (%s after start)", now.Sub(a.Status().StartedAt).Round(time.Millisecond))
		a.replyStart(nil)
		return
	}
	a.pendingErr = fmt.Errorf("health check failed: %w", ev.err)
	a.log.Writef("%v; killing", a.pendingErr)
	a.killGroup() // the exit event drives the failure policy
}

func (a *App) handleExit(ev exitEvent) {
	if ev.gen != a.gen {
		return
	}
	if a.healthCancel != nil {
		a.healthCancel()
		a.healthCancel = nil
	}
	desc := describeExit(ev)
	// The main process is gone; nothing else of this app may keep running.
	a.killGroup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := a.group.WaitEmpty(ctx); err != nil {
		a.slog.Error("app cgroup not empty after kill", "err", err)
	}
	cancel()
	oom := a.checkOOM()
	if oom {
		desc += " (out of memory)"
	}
	a.cmd = nil
	now := time.Now()
	a.setStatus(func(s *Status) { s.PID, s.LastExit, s.LastExitAt = 0, desc, now })
	a.log.Writef("exited: %s", desc)

	switch a.state() {
	case Stopping:
		if a.stopTimer != nil {
			a.stopTimer.Stop()
			a.stopTimer = nil
		}
		a.setStatus(func(s *Status) { s.State = Stopped })
		a.log.Writef("stopped")
		for _, w := range a.stopWaiters {
			w <- nil
		}
		a.stopWaiters = nil
	case Starting:
		err := a.pendingErr
		if err == nil {
			err = fmt.Errorf("exited before becoming healthy: %s", desc)
		}
		a.setStatus(func(s *Status) { s.LastError = err.Error() })
		a.failure(err)
	case Running:
		err := fmt.Errorf("exited unexpectedly: %s", desc)
		a.setStatus(func(s *Status) { s.LastError = err.Error() })
		a.failure(err)
	}
}

// failure applies the restart policy after a failed start or crash.
func (a *App) failure(err error) {
	a.replyStart(err)
	if a.desired != Running {
		a.setStatus(func(s *Status) { s.State = Stopped })
		return
	}
	now := time.Now()
	if h := a.Status().HealthyAt; !h.IsZero() && now.Sub(h) >= a.policy.StableAfter {
		a.consecutive = 0
	}
	a.exits = append(a.exits, now)
	cut := 0
	for cut < len(a.exits) && now.Sub(a.exits[cut]) > a.policy.CrashWindow {
		cut++
	}
	a.exits = a.exits[cut:]
	if len(a.exits) >= a.policy.CrashLimit {
		a.setStatus(func(s *Status) { s.State, s.NextStart = Crashed, time.Time{} })
		a.log.Writef("crashed: %d failures within %s; not restarting until started manually", len(a.exits), a.policy.CrashWindow)
		a.slog.Error("app crashed; automatic restarts stopped", "failures", len(a.exits))
		return
	}
	delay := a.policy.BackoffMin << a.consecutive
	if delay > a.policy.BackoffMax || delay <= 0 {
		delay = a.policy.BackoffMax
	}
	a.consecutive++
	a.backoff = time.NewTimer(delay)
	next := now.Add(delay)
	a.setStatus(func(s *Status) { s.State, s.NextStart = Backoff, next; s.Restarts++ })
	a.log.Writef("restarting in %s", delay)
}

func (a *App) replyStart(err error) {
	for _, w := range a.startWaiters {
		w <- err
	}
	a.startWaiters = nil
}

func (a *App) killGroup() {
	if err := a.group.Kill(); err != nil {
		a.slog.Error("killing app cgroup failed", "err", err)
	}
}

// checkOOM records new OOM kills; it reports whether any occurred.
func (a *App) checkOOM() bool {
	st, err := a.group.Stats()
	if err != nil || st.OOMKills <= a.lastOOM {
		return false
	}
	delta := st.OOMKills - a.lastOOM
	a.lastOOM = st.OOMKills
	a.setStatus(func(s *Status) { s.OOMKills += delta })
	a.log.Writef("OOM: kernel killed %d process(es) at the %s memory limit", delta, formatBytes(a.Spec().Limits.MemoryMax))
	a.slog.Warn("app hit its memory limit", "oom_kills", delta)
	return true
}

func describeExit(ev exitEvent) string {
	ps := ev.state
	if ps == nil {
		if ev.err != nil {
			return ev.err.Error()
		}
		return "unknown"
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "killed by signal " + unix.SignalName(ws.Signal())
	}
	return "exit code " + strconv.Itoa(ps.ExitCode())
}

func joinArgs(args []string) string {
	s := ""
	for i, a := range args {
		if i > 0 {
			s += " "
		}
		s += a
	}
	return s
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
