// Package builder runs an app's build command as the app user inside the
// app's build cgroup, with the pinned Zig first on PATH
// (docs/architecture.md §11, docs/app-contract.md §2 "Build environment").
package builder

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sumitwaani2/dootd/internal/cgroup"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/users"
)

// Defaults (docs/app-contract.md §2).
const (
	DefaultMemoryMax = 1 << 30
	DefaultTimeout   = 15 * time.Minute
	buildPidsMax     = 2048
	buildCPUWeight   = 50 // app runtimes use the default 100, so builds yield
)

// Builder runs builds.
type Builder struct {
	Layout  layout.Layout
	Cgroups *cgroup.Manager
}

// Job describes one build.
type Job struct {
	App       string
	User      users.User
	Workspace string // checkout root
	Subdir    string // app root inside the checkout ("" = checkout root)
	Command   string // run with /bin/sh -c
	ZigDir    string // directory containing the pinned zig binary
	Env       map[string]string
	MemoryMax int64
	Timeout   time.Duration
	Log       *logs.Log
}

// ErrTimeout is returned when the build exceeds its time limit.
var ErrTimeout = errors.New("build timed out")

// AppRoot returns the directory the build (and later the app) runs in.
func (j Job) AppRoot() string { return filepath.Join(j.Workspace, j.Subdir) }

// Run executes the build. On return no build process is left running.
func (b *Builder) Run(ctx context.Context, j Job) error {
	if j.MemoryMax <= 0 {
		j.MemoryMax = DefaultMemoryMax
	}
	if j.Timeout <= 0 {
		j.Timeout = DefaultTimeout
	}
	root := j.AppRoot()
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return fmt.Errorf("app path %q does not exist in the repository", j.Subdir)
	}
	if err := chownTree(j.Workspace, j.User); err != nil {
		return err
	}
	cache, err := b.prepareCache(j)
	if err != nil {
		return err
	}

	g, err := b.Cgroups.Group(cgroup.Builds, j.App)
	if err != nil {
		return err
	}
	if _, err := g.SetLimits(cgroup.Limits{MemoryMax: j.MemoryMax, NoSwap: true, CPUWeight: buildCPUWeight, PidsMax: buildPidsMax}); err != nil {
		return err
	}
	// A previous interrupted build must not linger.
	if err := g.Kill(); err != nil {
		return err
	}
	fd, err := g.OpenFD()
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", j.Command)
	cmd.Dir = root
	cmd.Env = environ(j, cache)
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &syscall.Credential{Uid: j.User.UID, Gid: j.User.GID, Groups: []uint32{}},
		Setpgid:     true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true,
		CgroupFD:    fd,
	}
	j.Log.Writef("$ %s   (in %s, as %s, memory %dM, timeout %s)", j.Command, displayDir(j.Subdir), j.User.Name, j.MemoryMax>>20, j.Timeout)
	start := time.Now()
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return fmt.Errorf("start build: %w", err)
	}
	copied := make(chan struct{}, 2)
	go func() { j.Log.Capture(logs.Stdout, outR); outR.Close(); copied <- struct{}{} }()
	go func() { j.Log.Capture(logs.Stderr, errR); errR.Close(); copied <- struct{}{} }()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	timer := time.NewTimer(j.Timeout)
	defer timer.Stop()
	var result error
	select {
	case err := <-waitErr:
		result = exitError(err)
	case <-timer.C:
		result = fmt.Errorf("%w after %s", ErrTimeout, j.Timeout)
		g.Kill()
		<-waitErr
	case <-ctx.Done():
		result = fmt.Errorf("build cancelled: %w", ctx.Err())
		g.Kill()
		<-waitErr
	}
	// Background processes started by the build die with it.
	g.Kill()
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.WaitEmpty(wctx); err != nil {
		return err
	}
	// Pipes close once every writer (including grandchildren) is gone.
	<-copied
	<-copied

	if st, err := g.Stats(); err == nil && st.OOMKills > 0 && result != nil {
		result = fmt.Errorf("%w (the build ran out of memory at %dM; raise the build memory limit)", result, j.MemoryMax>>20)
	}
	if result == nil {
		j.Log.Writef("build finished in %s", time.Since(start).Round(100*time.Millisecond))
	}
	// Remove the group so OOM counters start fresh next time.
	g.Remove()
	return result
}

func displayDir(sub string) string {
	if sub == "" {
		return "repo root"
	}
	return sub
}

func exitError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fmt.Errorf("build killed by signal %s", unix.SignalName(ws.Signal()))
		}
		return fmt.Errorf("build failed with exit code %d", ee.ExitCode())
	}
	return fmt.Errorf("build: %w", err)
}

type cacheDirs struct{ global, local, home, tmp string }

func (b *Builder) prepareCache(j Job) (cacheDirs, error) {
	base := b.Layout.ZigCacheDir(j.App)
	c := cacheDirs{
		global: filepath.Join(base, "global"),
		local:  filepath.Join(base, "local"),
		home:   filepath.Join(base, "home"),
		tmp:    filepath.Join(base, "tmp"),
	}
	os.RemoveAll(c.tmp)
	for _, d := range []string{c.global, c.local, c.home, c.tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return c, err
		}
		if err := os.Lchown(d, int(j.User.UID), int(j.User.GID)); err != nil {
			return c, err
		}
	}
	return c, nil
}

func environ(j Job, c cacheDirs) []string {
	env := map[string]string{}
	for k, v := range j.Env {
		env[k] = v
	}
	for k, v := range map[string]string{
		"PATH":                 j.ZigDir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME":                 c.home,
		"TMPDIR":               c.tmp,
		"LANG":                 "C.UTF-8",
		"CC":                   "zig cc",
		"CXX":                  "zig c++",
		"ZIG_GLOBAL_CACHE_DIR": c.global,
		"ZIG_LOCAL_CACHE_DIR":  c.local,
		"DOOTD_APP":            j.App,
		"DOOTD_BUILD":          "1",
	} {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// chownTree gives the freshly cloned (root-owned) tree to the app user.
// It never follows symlinks.
func chownTree(dir string, u users.User) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, int(u.UID), int(u.GID))
	})
}

// Finalize makes a built tree immutable for the app: every entry the app
// user owns becomes root-owned, directories 0755 and files 0644/0755
// (setuid/setgid bits dropped). Symlinks are never followed, and entries
// owned by anyone else are left untouched. Must only run once no build
// process is left.
func Finalize(dir string, u users.User) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		if st.Uid != u.UID {
			return nil
		}
		if err := os.Lchown(p, 0, 0); err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return nil
		case d.IsDir():
			return os.Chmod(p, 0o755)
		case d.Type().IsRegular():
			mode := os.FileMode(0o644)
			if st.Mode&0o111 != 0 {
				mode = 0o755
			}
			return os.Chmod(p, mode)
		default: // fifos, sockets: not needed in a release
			return os.Remove(p)
		}
	})
}

// CheckBinary verifies the run binary exists inside root and is executable.
func CheckBinary(root, bin string) error {
	p := filepath.Join(root, bin)
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("run binary %s was not produced by the build", bin)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if real != rootReal && !strings.HasPrefix(real, rootReal+"/") {
		return fmt.Errorf("run binary %s points outside the app directory", bin)
	}
	st, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("run binary %s is not an executable file", bin)
	}
	return nil
}

// RemoveGroup deletes an app's (empty) build cgroup.
func RemoveGroup(b *Builder, appName string) error {
	g, err := b.Cgroups.Group(cgroup.Builds, appName)
	if err != nil {
		return err
	}
	return g.Remove()
}
