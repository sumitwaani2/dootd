// Package testenv reads the DOOTD_TEST_* environment variables that the
// end-to-end scripts set in a systemd drop-in to run dootd against fakes
// (docs/architecture.md §18). They are not a user feature: on a real server
// none of them is set and every value is the fixed default.
package testenv

import (
	"os"
	"strconv"
	"time"
)

// CloudflareAPI is the Cloudflare API base URL override ("" = the real API).
func CloudflareAPI() string { return os.Getenv("DOOTD_TEST_CLOUDFLARE_API") }

// PublicIPv4 skips IPv4 detection when set.
func PublicIPv4() string { return os.Getenv("DOOTD_TEST_PUBLIC_IPV4") }

// PublicIPv6 skips IPv6 detection when set ("off" = no IPv6).
func PublicIPv6() string { return os.Getenv("DOOTD_TEST_PUBLIC_IPV6") }

// BackupInterval returns the test backup interval, or def.
func BackupInterval(def time.Duration) time.Duration {
	return duration("DOOTD_TEST_BACKUP_INTERVAL", def)
}

// BackupRetention returns the test backup retention, or def.
func BackupRetention(def time.Duration) time.Duration {
	return duration("DOOTD_TEST_BACKUP_RETENTION", def)
}

// WarnPercent returns the test disk/memory warning threshold, or def.
func WarnPercent(def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv("DOOTD_TEST_WARN_PERCENT"), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func duration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil && d >= time.Second {
		return d
	}
	return def
}
