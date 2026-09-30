// Package app defines the runtime description of a hosted app and the
// validation rules shared by the dev config, the dashboard and the deployer.
package app

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ContractVersion is the app contract version dootd implements
// (docs/app-contract.md).
const ContractVersion = 2

// Default resource limits (docs/app-contract.md §8).
const (
	DefaultMemoryMax  = 256 << 20
	DefaultCPUMax     = 1.0
	DefaultPidsMax    = 256
	DefaultNoFile     = 4096
	DefaultHealthPath = "/"
)

// Limits are the per-app runtime resource limits.
type Limits struct {
	MemoryMax int64   // bytes; memory.high is set to 90% of this
	CPUMax    float64 // cores, e.g. 1.0 or 0.5
	PidsMax   int
}

// Spec is everything the supervisor needs to run one app.
type Spec struct {
	Name       string
	Domain     string // informational until Phase 3
	Port       int
	ReleaseID  string
	ReleaseDir string   // working directory; binary paths are relative to it
	Run        []string // argv; Run[0] is relative to ReleaseDir unless absolute
	HealthPath string
	Env        map[string]string // user env vars (reserved names rejected)
	Limits     Limits
}

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,22}[a-z0-9]$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ReservedEnv are env names set by dootd that users may not override.
var ReservedEnv = []string{"PORT", "HOST", "DATA_DIR", "TMPDIR"}

// ValidateName checks an app name: 2-24 chars, lowercase letters, digits and
// hyphens, starting with a letter and not ending with a hyphen. The limit
// keeps the system user name "dootd-<name>" within 30 characters.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid app name %q: use 2-24 characters of a-z, 0-9 and '-', starting with a letter and not ending with '-'", name)
	}
	return nil
}

// NameFromRepo derives the app name from a repository name (Req 8.2):
// lowercased, with '_' and '.' turned into '-'. The result must still pass
// ValidateName.
func NameFromRepo(repo string) (string, error) {
	n := strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(repo))
	if err := ValidateName(n); err != nil {
		return "", fmt.Errorf("the repository name %q cannot be used as an app name (%q): app names are 2-24 characters of a-z, 0-9 and '-', starting with a letter and not ending with '-'", repo, n)
	}
	return n, nil
}

// ValidateEnvName checks a user-supplied env var name.
func ValidateEnvName(name string) error {
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("invalid env var name %q: use letters, digits and '_', not starting with a digit", name)
	}
	if strings.HasPrefix(name, "DOOTD_") {
		return fmt.Errorf("env var %q is reserved (names starting with DOOTD_ are set by dootd)", name)
	}
	for _, r := range ReservedEnv {
		if name == r {
			return fmt.Errorf("env var %q is reserved (set by dootd)", name)
		}
	}
	return nil
}

// Validate checks a Spec for internal consistency.
func (s *Spec) Validate() error {
	var errs []error
	if err := ValidateName(s.Name); err != nil {
		errs = append(errs, err)
	}
	if s.Port < 1024 || s.Port > 65535 {
		errs = append(errs, fmt.Errorf("app %q: port %d out of range 1024-65535", s.Name, s.Port))
	}
	// An app without a release (never deployed) has no release dir or run
	// command yet.
	if s.ReleaseID != "" {
		if !strings.HasPrefix(s.ReleaseDir, "/") {
			errs = append(errs, fmt.Errorf("app %q: release dir must be an absolute path", s.Name))
		}
		if len(s.Run) == 0 || s.Run[0] == "" {
			errs = append(errs, fmt.Errorf("app %q: run command is empty", s.Name))
		}
	}
	if s.HealthPath == "" {
		s.HealthPath = DefaultHealthPath
	}
	if !strings.HasPrefix(s.HealthPath, "/") {
		errs = append(errs, fmt.Errorf("app %q: health path must start with '/'", s.Name))
	}
	for k := range s.Env {
		if err := ValidateEnvName(k); err != nil {
			errs = append(errs, fmt.Errorf("app %q: %w", s.Name, err))
		}
	}
	if s.Limits.MemoryMax < 16<<20 {
		errs = append(errs, fmt.Errorf("app %q: memory limit must be at least 16M", s.Name))
	}
	if s.Limits.CPUMax <= 0 || s.Limits.CPUMax > 64 {
		errs = append(errs, fmt.Errorf("app %q: cpu limit must be between 0 and 64 cores", s.Name))
	}
	if s.Limits.PidsMax < 16 {
		errs = append(errs, fmt.Errorf("app %q: pids limit must be at least 16", s.Name))
	}
	return errors.Join(errs...)
}

// ParseBytes parses sizes like "256M", "1G", "512K" or a plain byte count.
// Units are binary (1K = 1024).
func ParseBytes(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, "B")
	t = strings.TrimSuffix(t, "I") // accept "Mi", "Gi"
	mult := int64(1)
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'K':
			mult, t = 1<<10, t[:n-1]
		case 'M':
			mult, t = 1<<20, t[:n-1]
		case 'G':
			mult, t = 1<<30, t[:n-1]
		}
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q (examples: 256M, 1G)", s)
	}
	return v * mult, nil
}

// SplitCommand splits a run command into argv. It supports single and
// double quotes and backslash escapes, but no shell features (no pipes,
// globbing, variables or &&), as specified by the app contract.
func SplitCommand(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inArg   bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inArg = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape in command %q", s)
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("command is empty")
	}
	return args, nil
}
