package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/sumitwaani2/dootd/internal/app"
)

// DevAppsFile is the temporary Phase 1-3 way to define apps that run from a
// prebuilt directory. It is removed once the dashboard exists (Phase 4).
//
//	[[app]]
//	name        = "sample-c"
//	type        = "c"
//	port        = 20001
//	release_dir = "/opt/dootd-dev/sample-c"
//	run         = "build/sample-c"
//	health_path = "/healthz"
//	memory      = "256M"
//	cpu         = 1.0
//	pids        = 256
//	[app.env]
//	GREETING = "hello"
type DevAppsFile struct {
	App []DevApp `toml:"app"`
}

// DevApp is one [[app]] entry.
type DevApp struct {
	Name       string            `toml:"name"`
	Type       string            `toml:"type"`
	Domain     string            `toml:"domain"`
	Port       int               `toml:"port"`
	ReleaseDir string            `toml:"release_dir"`
	Run        string            `toml:"run"`
	HealthPath string            `toml:"health_path"`
	Memory     string            `toml:"memory"`
	CPU        float64           `toml:"cpu"`
	Pids       int               `toml:"pids"`
	Env        map[string]string `toml:"env"`
}

// LoadDevApps parses and validates a dev apps file.
func LoadDevApps(path string) ([]app.Spec, error) {
	var f DevAppsFile
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return nil, fmt.Errorf("dev apps: %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("dev apps: %s: unknown key %q", path, und[0].String())
	}

	var (
		specs []app.Spec
		errs  []error
		names = map[string]bool{}
		ports = map[int]string{}
	)
	for i, d := range f.App {
		s := app.Spec{
			Name:       d.Name,
			Type:       app.Type(d.Type),
			Domain:     d.Domain,
			Port:       d.Port,
			ReleaseID:  "dev",
			ReleaseDir: d.ReleaseDir,
			HealthPath: d.HealthPath,
			Env:        d.Env,
			Limits:     app.Limits{MemoryMax: app.DefaultMemoryMax, CPUMax: d.CPU, PidsMax: d.Pids},
		}
		if s.HealthPath == "" {
			s.HealthPath = app.DefaultHealthPath
		}
		if s.Limits.CPUMax == 0 {
			s.Limits.CPUMax = app.DefaultCPUMax
		}
		if s.Limits.PidsMax == 0 {
			s.Limits.PidsMax = app.DefaultPidsMax
		}
		if d.Memory != "" {
			if s.Limits.MemoryMax, err = app.ParseBytes(d.Memory); err != nil {
				errs = append(errs, fmt.Errorf("app #%d (%s): memory: %w", i+1, d.Name, err))
			}
		}
		if s.Run, err = app.SplitCommand(d.Run); err != nil {
			errs = append(errs, fmt.Errorf("app #%d (%s): run: %w", i+1, d.Name, err))
			s.Run = []string{"-"} // already reported; avoid a duplicate "empty" error
		}
		if err := s.Validate(); err != nil {
			errs = append(errs, err)
		}
		if names[s.Name] {
			errs = append(errs, fmt.Errorf("app %q is defined twice", s.Name))
		}
		names[s.Name] = true
		if other, ok := ports[s.Port]; ok {
			errs = append(errs, fmt.Errorf("apps %q and %q both use port %d", other, s.Name, s.Port))
		}
		ports[s.Port] = s.Name
		if st, err := os.Stat(s.ReleaseDir); err != nil || !st.IsDir() {
			errs = append(errs, fmt.Errorf("app %q: release_dir %q is not a directory", s.Name, s.ReleaseDir))
		}
		specs = append(specs, s)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("dev apps: %s:\n%w", path, err)
	}
	return specs, nil
}
