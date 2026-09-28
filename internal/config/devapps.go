package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/sumitwaani2/dootd/internal/app"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/github"
)

// DevAppsFile is the temporary (Phases 1-3) way to define apps. It is
// replaced by the dashboard in Phase 4. Two kinds of entries:
//
// Deployable from git (Phase 2), deployed with `dootd ctl deploy <name>`:
//
//	[[app]]
//	name          = "blog"
//	type          = "zig"
//	port          = 20001
//	repo          = "https://github.com/you/blog"   # or file:///path/to/repo.git
//	branch        = "main"                          # default "main"
//	path          = ""                              # app root inside the repo (default: repo root)
//	build_memory  = "1G"
//	build_timeout = "15m"
//	memory        = "256M"
//	[app.env]
//	GREETING = "hello"
//
// Prebuilt (Phase 1 style), run straight from a directory:
//
//	[[app]]
//	name        = "sample-c"
//	type        = "c"
//	port        = 20002
//	release_dir = "/opt/dootd-dev/sample-c"
//	run         = "build/sample-c"
//	health_path = "/healthz"
type DevAppsFile struct {
	App []DevApp `toml:"app"`
}

// DevApp is one [[app]] entry.
type DevApp struct {
	Name         string            `toml:"name"`
	Type         string            `toml:"type"`
	Domain       string            `toml:"domain"`
	Port         int               `toml:"port"`
	Repo         string            `toml:"repo"`
	Branch       string            `toml:"branch"`
	Path         string            `toml:"path"`
	BuildMemory  string            `toml:"build_memory"`
	BuildTimeout string            `toml:"build_timeout"`
	ReleaseDir   string            `toml:"release_dir"`
	Run          string            `toml:"run"`
	HealthPath   string            `toml:"health_path"`
	Memory       string            `toml:"memory"`
	CPU          float64           `toml:"cpu"`
	Pids         int               `toml:"pids"`
	Env          map[string]string `toml:"env"`
}

// LoadedApp is a validated dev app entry.
type LoadedApp struct {
	Spec app.Spec
	// Deployable apps (Repo.URL != ""):
	Repo         github.Repo
	Branch       string
	Subdir       string
	BuildMemory  int64
	BuildTimeout time.Duration
}

// Deployable reports whether the app is built from git.
func (l LoadedApp) Deployable() bool { return l.Repo.URL != "" }

// LoadDevApps parses and validates a dev apps file, reporting every problem.
func LoadDevApps(path string) ([]LoadedApp, error) {
	var f DevAppsFile
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return nil, fmt.Errorf("dev apps: %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("dev apps: %s: unknown key %q", path, und[0].String())
	}

	var (
		out     []LoadedApp
		errs    []error
		names   = map[string]bool{}
		ports   = map[int]string{}
		domains = map[string]string{}
	)
	for i, d := range f.App {
		fail := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("app #%d (%s): %s", i+1, d.Name, fmt.Sprintf(format, a...)))
		}
		s := app.Spec{
			Name:       d.Name,
			Type:       app.Type(d.Type),
			Domain:     d.Domain,
			Port:       d.Port,
			HealthPath: d.HealthPath,
			Env:        d.Env,
			Limits:     app.Limits{MemoryMax: app.DefaultMemoryMax, CPUMax: d.CPU, PidsMax: d.Pids},
		}
		if s.Limits.CPUMax == 0 {
			s.Limits.CPUMax = app.DefaultCPUMax
		}
		if s.Limits.PidsMax == 0 {
			s.Limits.PidsMax = app.DefaultPidsMax
		}
		if d.Memory != "" {
			if s.Limits.MemoryMax, err = app.ParseBytes(d.Memory); err != nil {
				fail("memory: %v", err)
			}
		}
		la := LoadedApp{}

		switch {
		case d.Repo != "" && (d.ReleaseDir != "" || d.Run != "" || d.HealthPath != ""):
			fail("use either repo (built by dootd; run and health_path come from dootd.toml) or release_dir + run, not both")
		case d.Repo != "":
			if la.Repo, err = github.ParseRepo(d.Repo); err != nil {
				fail("%v", err)
			}
			la.Branch = d.Branch
			if la.Branch == "" {
				la.Branch = "main"
			}
			if err := github.ValidateBranch(la.Branch); err != nil {
				fail("%v", err)
			}
			if la.Subdir, err = cleanSubdir(d.Path); err != nil {
				fail("path: %v", err)
			}
			if d.BuildMemory != "" {
				if la.BuildMemory, err = app.ParseBytes(d.BuildMemory); err != nil {
					fail("build_memory: %v", err)
				}
			}
			if d.BuildTimeout != "" {
				if la.BuildTimeout, err = time.ParseDuration(d.BuildTimeout); err != nil || la.BuildTimeout < time.Second {
					fail("build_timeout %q: use a duration like 15m", d.BuildTimeout)
				}
			}
		default:
			if d.Branch != "" || d.Path != "" || d.BuildMemory != "" || d.BuildTimeout != "" {
				fail("branch, path, build_memory and build_timeout only apply to repo apps")
			}
			s.ReleaseID = "dev"
			s.ReleaseDir = d.ReleaseDir
			if s.Run, err = app.SplitCommand(d.Run); err != nil {
				fail("run: %v", err)
				s.Run = []string{"-"} // already reported; avoid a duplicate "empty" error
			}
			if st, err := os.Stat(s.ReleaseDir); err != nil || !st.IsDir() {
				fail("release_dir %q is not a directory", s.ReleaseDir)
			}
		}
		if err := s.Validate(); err != nil {
			errs = append(errs, err)
		}
		if s.Domain != "" {
			s.Domain = strings.ToLower(s.Domain)
			if err := edge.ValidHostname(s.Domain); err != nil {
				fail("%v", err)
			} else if other, ok := domains[s.Domain]; ok {
				errs = append(errs, fmt.Errorf("apps %q and %q both use domain %s", other, s.Name, s.Domain))
			}
			domains[s.Domain] = s.Name
		}
		if names[s.Name] {
			errs = append(errs, fmt.Errorf("app %q is defined twice", s.Name))
		}
		names[s.Name] = true
		if other, ok := ports[s.Port]; ok {
			errs = append(errs, fmt.Errorf("apps %q and %q both use port %d", other, s.Name, s.Port))
		}
		ports[s.Port] = s.Name
		la.Spec = s
		out = append(out, la)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("dev apps: %s:\n%w", path, err)
	}
	return out, nil
}

func cleanSubdir(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	c := filepath.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, ".git") {
		return "", fmt.Errorf("%q must be a directory inside the repository", p)
	}
	if c == "." {
		return "", nil
	}
	return c, nil
}
