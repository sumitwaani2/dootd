// Package config loads dootd's static configuration (/etc/dootd/config.toml)
// and, during development phases, the temporary dev apps file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	Edge       Edge       `toml:"edge"`
	Backups    Backups    `toml:"backups"`
	Monitoring Monitoring `toml:"monitoring"`
}

// Monitoring configures dashboard warning thresholds (docs/architecture.md §13).
type Monitoring struct {
	DiskWarnPercent   float64 `toml:"disk_warn_percent"`   // default 85
	MemoryWarnPercent float64 `toml:"memory_warn_percent"` // default 90
	CertWarnDays      int     `toml:"cert_warn_days"`      // default 14
}

// Backups configures the backup schedule (docs/architecture.md §12).
type Backups struct {
	Interval  Duration `toml:"interval"`  // default 3h
	Retention Duration `toml:"retention"` // default 48h
}

// Duration is a time.Duration written like "3h" in TOML.
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Edge configures the public HTTPS listener (docs/architecture.md §9).
type Edge struct {
	Listen          string `toml:"listen"`           // default ":443"; "off" disables the edge
	DashboardDomain string `toml:"dashboard_domain"` // e.g. "dootd.example.com"
	PublicIPv4      string `toml:"public_ipv4"`      // default: auto-detect
	PublicIPv6      string `toml:"public_ipv6"`      // default: auto-detect; "off" = no AAAA records
	AOP             *bool  `toml:"authenticated_origin_pulls"`
	CloudflareAPI   string `toml:"cloudflare_api"` // API base override (tests only)
}

// AOPEnabled reports whether zone-level Authenticated Origin Pulls are on (default true).
func (e Edge) AOPEnabled() bool { return e.AOP == nil || *e.AOP }

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
	if b := c.Backups; (b.Interval.Duration != 0 && b.Interval.Duration < 10*time.Second) ||
		(b.Retention.Duration != 0 && b.Retention.Duration < b.Interval.Duration) {
		return nil, fmt.Errorf("config: backups.interval must be at least 10s and backups.retention at least the interval")
	}
	if c.Edge.Listen == "" {
		c.Edge.Listen = ":443"
	}
	c.Edge.DashboardDomain = strings.ToLower(strings.TrimSpace(c.Edge.DashboardDomain))
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
