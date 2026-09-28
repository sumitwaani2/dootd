// Package cgroup manages dootd's cgroup v2 subtree.
//
// Under systemd with Delegate=yes dootd owns its service cgroup. Because
// cgroup v2 forbids processes in non-leaf groups ("no internal processes"),
// dootd first moves itself into a leaf and then builds this tree:
//
//	<root>/                 dootd.service (delegated)
//	  supervisor/           dootd itself
//	  apps/<app>/           app runtime processes
//	  builds/<app>/         build processes
package cgroup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Mount is where the unified cgroup v2 hierarchy is mounted.
const Mount = "/sys/fs/cgroup"

// Kind is a top-level group under the dootd root.
type Kind string

// Group kinds.
const (
	Apps   Kind = "apps"
	Builds Kind = "builds"
)

// wantedControllers are enabled wherever the kernel offers them.
var wantedControllers = []string{"cpu", "memory", "pids", "io"}

// Manager owns the dootd cgroup subtree.
type Manager struct {
	root        string
	controllers map[string]bool // controllers enabled for app/build groups
}

// Setup prepares the subtree. rootOverride may be empty to use the cgroup
// dootd is currently running in.
func Setup(rootOverride string) (*Manager, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(Mount, &st); err != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, fmt.Errorf("cgroup: %s is not a cgroup v2 (unified) mount", Mount)
	}

	root := rootOverride
	if root == "" {
		self, err := selfCgroup()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(Mount, self)
	}
	if !strings.HasPrefix(filepath.Clean(root)+"/", Mount+"/") {
		return nil, fmt.Errorf("cgroup: root %q is not under %s", root, Mount)
	}
	m := &Manager{root: filepath.Clean(root), controllers: map[string]bool{}}

	// 1. Move ourselves into a leaf.
	sup := filepath.Join(m.root, "supervisor")
	if err := os.MkdirAll(sup, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup: create %s: %w", sup, err)
	}
	if err := writeFile(filepath.Join(sup, "cgroup.procs"), strconv.Itoa(os.Getpid())); err != nil {
		return nil, fmt.Errorf("cgroup: move dootd into %s: %w", sup, err)
	}

	// 2. Enable controllers at the root, then in apps/ and builds/.
	if err := m.enableControllers(m.root); err != nil {
		return nil, err
	}
	for _, k := range []Kind{Apps, Builds} {
		dir := filepath.Join(m.root, string(k))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cgroup: create %s: %w", dir, err)
		}
		if err := m.enableControllers(dir); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func selfCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("cgroup: read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return p, nil
		}
	}
	return "", errors.New("cgroup: no cgroup v2 entry in /proc/self/cgroup")
}

// enableControllers enables every wanted controller the parent offers in
// dir's subtree_control and records what ended up enabled.
func (m *Manager) enableControllers(dir string) error {
	avail, err := readFields(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup: %w", err)
	}
	for _, c := range wantedControllers {
		if !avail[c] {
			continue
		}
		if err := writeFile(filepath.Join(dir, "cgroup.subtree_control"), "+"+c); err != nil {
			if errors.Is(err, syscall.EBUSY) {
				return fmt.Errorf("cgroup: cannot enable controllers in %s because it still contains other processes; "+
					"run dootd as a systemd service with Delegate=yes (see contrib/systemd/dootd.service)", dir)
			}
			return fmt.Errorf("cgroup: enable %s in %s: %w", c, dir, err)
		}
	}
	enabled, err := readFields(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("cgroup: %w", err)
	}
	m.controllers = enabled
	return nil
}

// Root returns the managed root directory.
func (m *Manager) Root() string { return m.root }

// Controllers reports which controllers are active for app and build groups.
func (m *Manager) Controllers() map[string]bool {
	out := make(map[string]bool, len(m.controllers))
	for k, v := range m.controllers {
		out[k] = v
	}
	return out
}

// Missing lists wanted controllers that are not available on this host.
func (m *Manager) Missing() []string {
	var out []string
	for _, c := range wantedControllers {
		if !m.controllers[c] {
			out = append(out, c)
		}
	}
	return out
}

// Group returns the handle for kind/name, creating the directory if needed.
func (m *Manager) Group(kind Kind, name string) (*Group, error) {
	if name == "" || strings.ContainsAny(name, "/.") {
		return nil, fmt.Errorf("cgroup: invalid group name %q", name)
	}
	dir := filepath.Join(m.root, string(kind), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup: create %s: %w", dir, err)
	}
	return &Group{Path: dir, controllers: m.controllers}, nil
}

// ReapStale kills processes left in any app or build group, e.g. after a
// dootd crash in a dev setup. It returns the names of groups it cleaned.
func (m *Manager) ReapStale(ctx context.Context) ([]string, error) {
	var cleaned []string
	for _, k := range []Kind{Apps, Builds} {
		entries, err := os.ReadDir(filepath.Join(m.root, string(k)))
		if err != nil {
			return cleaned, fmt.Errorf("cgroup: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			g := &Group{Path: filepath.Join(m.root, string(k), e.Name()), controllers: m.controllers}
			if pop, _ := g.Populated(); !pop {
				continue
			}
			if err := g.Kill(); err != nil {
				return cleaned, err
			}
			if err := g.WaitEmpty(ctx); err != nil {
				return cleaned, err
			}
			cleaned = append(cleaned, string(k)+"/"+e.Name())
		}
	}
	return cleaned, nil
}

// Limits for a group. Zero values leave the kernel default in place.
type Limits struct {
	MemoryMax  int64   // bytes (memory.max)
	MemoryHigh int64   // bytes (memory.high, throttling threshold)
	NoSwap     bool    // memory.swap.max = 0
	CPUMax     float64 // cores (cpu.max)
	CPUWeight  int     // 1-10000, default 100 (cpu.weight)
	PidsMax    int     // pids.max
}

// Group is one leaf cgroup.
type Group struct {
	Path        string
	controllers map[string]bool
}

// SetLimits applies l. Limits whose controller is unavailable are skipped
// and reported in the returned list so callers can warn about them.
func (g *Group) SetLimits(l Limits) (skipped []string, err error) {
	set := func(ctrl, file, val string) error {
		if !g.controllers[ctrl] {
			skipped = append(skipped, file)
			return nil
		}
		p := filepath.Join(g.Path, file)
		if err := writeFile(p, val); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				skipped = append(skipped, file)
				return nil
			}
			return fmt.Errorf("cgroup: set %s=%s: %w", p, val, err)
		}
		return nil
	}

	var errs []error
	if l.MemoryMax > 0 {
		errs = append(errs, set("memory", "memory.max", strconv.FormatInt(l.MemoryMax, 10)))
	}
	if l.MemoryHigh > 0 {
		errs = append(errs, set("memory", "memory.high", strconv.FormatInt(l.MemoryHigh, 10)))
	}
	if l.NoSwap {
		errs = append(errs, set("memory", "memory.swap.max", "0"))
		if _, err := os.Stat(filepath.Join(g.Path, "memory.zswap.max")); err == nil {
			errs = append(errs, set("memory", "memory.zswap.max", "0"))
		}
	}
	if l.CPUMax > 0 {
		const period = 100000
		quota := int64(l.CPUMax * period)
		errs = append(errs, set("cpu", "cpu.max", fmt.Sprintf("%d %d", quota, period)))
	}
	if l.CPUWeight > 0 {
		errs = append(errs, set("cpu", "cpu.weight", strconv.Itoa(l.CPUWeight)))
	}
	if l.PidsMax > 0 {
		errs = append(errs, set("pids", "pids.max", strconv.Itoa(l.PidsMax)))
	}
	return skipped, errors.Join(errs...)
}

// OpenFD opens the group directory for use as SysProcAttr.CgroupFD
// (clone3 CLONE_INTO_CGROUP). The caller must close it.
func (g *Group) OpenFD() (int, error) {
	fd, err := unix.Open(g.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("cgroup: open %s: %w", g.Path, err)
	}
	return fd, nil
}

// Procs lists the PIDs in the group.
func (g *Group) Procs() ([]int, error) {
	b, err := os.ReadFile(filepath.Join(g.Path, "cgroup.procs"))
	if err != nil {
		return nil, fmt.Errorf("cgroup: %w", err)
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(f); err == nil {
			pids = append(pids, p)
		}
	}
	return pids, nil
}

// Populated reports whether any process is in the group.
func (g *Group) Populated() (bool, error) {
	kv, err := readKV(filepath.Join(g.Path, "cgroup.events"))
	if err != nil {
		return false, fmt.Errorf("cgroup: %w", err)
	}
	return kv["populated"] != 0, nil
}

// Kill SIGKILLs every process in the group. It repeats until a pass finds
// no new processes, so children forked during the kill are caught too.
//
// It deliberately does NOT use cgroup.kill: on the Ubuntu 24.04 kernels we
// tested, once cgroup.kill had been written for a group, every later child
// spawned into that group with clone3(CLONE_INTO_CGROUP) was SIGKILLed right
// away, which broke restarts. See scripts/e2e/phase1.sh (crash loop test).
func (g *Group) Kill() error {
	signaled := map[int]bool{}
	for pass := 0; pass < 20; pass++ {
		pids, err := g.Procs()
		if err != nil {
			return err
		}
		fresh := 0
		for _, p := range pids {
			if signaled[p] {
				continue
			}
			signaled[p] = true
			fresh++
			if err := syscall.Kill(p, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("cgroup: kill pid %d in %s: %w", p, g.Path, err)
			}
		}
		if fresh == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("cgroup: %s keeps spawning processes while being killed", g.Path)
}

// WaitEmpty blocks until the group has no processes or ctx ends.
func (g *Group) WaitEmpty(ctx context.Context) error {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		pop, err := g.Populated()
		if err != nil {
			return err
		}
		if !pop {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cgroup: %s still has processes: %w", g.Path, ctx.Err())
		case <-t.C:
		}
	}
}

// Stats is a point-in-time reading of a group's counters.
type Stats struct {
	MemoryCurrent int64 // bytes
	CPUUsageUsec  int64 // cumulative
	PidsCurrent   int64
	OOMKills      int64 // cumulative memory.events oom_kill
	IOReadBytes   int64 // cumulative, all devices (io.stat rbytes)
	IOWriteBytes  int64 // cumulative, all devices (io.stat wbytes)
}

// Stats reads the group's counters. Files of unavailable controllers read as 0.
func (g *Group) Stats() (Stats, error) {
	var s Stats
	var err error
	if s.MemoryCurrent, err = readInt(filepath.Join(g.Path, "memory.current")); err != nil {
		return s, err
	}
	if s.PidsCurrent, err = readInt(filepath.Join(g.Path, "pids.current")); err != nil {
		return s, err
	}
	cpu, err := readKV(filepath.Join(g.Path, "cpu.stat"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, fmt.Errorf("cgroup: %w", err)
	}
	s.CPUUsageUsec = cpu["usage_usec"]
	ev, err := readKV(filepath.Join(g.Path, "memory.events"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, fmt.Errorf("cgroup: %w", err)
	}
	s.OOMKills = ev["oom_kill"]
	s.IOReadBytes, s.IOWriteBytes = readIOStat(filepath.Join(g.Path, "io.stat"))
	return s, nil
}

// readIOStat sums rbytes/wbytes over all devices in io.stat
// ("8:0 rbytes=1 wbytes=2 rios=3 ..." per line).
func readIOStat(path string) (r, w int64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	for _, f := range strings.Fields(string(b)) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "rbytes":
			r += n
		case "wbytes":
			w += n
		}
	}
	return r, w
}

// Remove deletes the (empty) group.
func (g *Group) Remove() error {
	if err := os.Remove(g.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cgroup: remove %s: %w", g.Path, err)
	}
	return nil
}

func writeFile(path, val string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(val)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func readFields(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, f := range strings.Fields(string(b)) {
		out[f] = true
	}
	return out, nil
}

// readKV parses "key value" lines (cgroup.events, cpu.stat, memory.events).
func readKV(path string) (map[string]int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]int64{}, err
	}
	out := map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out, nil
}

// readInt reads a single-integer file; missing files and "max" read as 0.
func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cgroup: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "max" {
		return 0, nil
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cgroup: parse %s: %w", path, err)
	}
	return n, nil
}
