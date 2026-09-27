// Package config loads dootd's static configuration (/etc/dootd/config.toml)
// and, during development phases, the temporary dev apps file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Default paths.
const (
	DefaultPath      = "/etc/dootd/config.toml"
	DefaultDataRoot  = "/var/lib/dootd"
	DefaultMasterKey = "/etc/dootd/master.key"
)

// Config is the static, rarely changing configuration. Everything else lives
// in dootd.db and is edited from the dashboard.
type Config struct {
	DataRoot  string `toml:"data_root"`
	MasterKey string `toml:"master_key"`
	// CgroupRoot overrides the cgroup directory dootd manages. Empty means
	// "the cgroup dootd runs in" (the systemd-delegated dootd.service cgroup).
	CgroupRoot string `toml:"cgroup_root"`
}

// Load reads the config at path. If the file does not exist and mustExist is
// false, defaults are returned.
func Load(path string, mustExist bool) (*Config, error) {
	c := &Config{DataRoot: DefaultDataRoot, MasterKey: DefaultMasterKey}
	md, err := toml.DecodeFile(path, c)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !mustExist:
		// defaults
	case err != nil:
		return nil, fmt.Errorf("config: %s: %w", path, err)
	default:
		if und := md.Undecoded(); len(und) > 0 {
			return nil, fmt.Errorf("config: %s: unknown key %q", path, und[0].String())
		}
	}
	for name, p := range map[string]string{"data_root": c.DataRoot, "master_key": c.MasterKey} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("config: %s must be an absolute path, got %q", name, p)
		}
	}
	if c.CgroupRoot != "" && !filepath.IsAbs(c.CgroupRoot) {
		return nil, fmt.Errorf("config: cgroup_root must be an absolute path, got %q", c.CgroupRoot)
	}
	return c, nil
}

// DBPath is the location of dootd.db.
func (c *Config) DBPath() string { return filepath.Join(c.DataRoot, "dootd.db") }

// EnsureDirs creates the base directories with their required modes.
// /var/lib/dootd is 0711 so app users can traverse to their own
// directories without being able to list anything.
func (c *Config) EnsureDirs() error {
	if err := os.MkdirAll(filepath.Dir(c.MasterKey), 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", filepath.Dir(c.MasterKey), err)
	}
	if err := os.MkdirAll(c.DataRoot, 0o711); err != nil {
		return fmt.Errorf("config: create %s: %w", c.DataRoot, err)
	}
	return os.Chmod(c.DataRoot, 0o711)
}
