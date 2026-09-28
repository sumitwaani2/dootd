// Package manifest parses and validates an app's dootd.toml
// (docs/app-contract.md §2).
package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sumitwaani2/dootd/internal/app"
)

// FileName is the manifest file name at the app root.
const FileName = "dootd.toml"

const maxSize = 64 << 10

// Manifest is a validated dootd.toml.
type Manifest struct {
	Contract   int
	ZigVersion string
	Build      string   // shell command, run with /bin/sh -c
	Run        []string // argv; Run[0] relative to the app root
	HealthPath string
}

type raw struct {
	Contract   *int    `toml:"contract"`
	ZigVersion *string `toml:"zig_version"`
	Build      *string `toml:"build"`
	Run        *string `toml:"run"`
	HealthPath *string `toml:"health_path"`
}

var zigVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// DefaultBuild returns the default build command for an app type.
func DefaultBuild(t app.Type) string {
	if t == app.TypeC {
		return "make"
	}
	return "zig build -Doptimize=ReleaseSafe"
}

// Load reads dir/dootd.toml and validates it for an app of type t.
func Load(dir string, t app.Type) (*Manifest, error) {
	p := filepath.Join(dir, FileName)
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s not found at the app root; see docs/app-contract.md", FileName)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", FileName)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSize {
		return nil, fmt.Errorf("%s is larger than 64 KB", FileName)
	}
	return Parse(b, t)
}

// Parse validates manifest content, reporting every problem at once.
func Parse(b []byte, t app.Type) (*Manifest, error) {
	var r raw
	md, err := toml.Decode(string(b), &r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	var errs []error
	for _, k := range md.Undecoded() {
		errs = append(errs, fmt.Errorf("unknown key %q", k.String()))
	}
	m := &Manifest{Build: DefaultBuild(t), HealthPath: app.DefaultHealthPath}

	switch {
	case r.Contract == nil:
		errs = append(errs, errors.New("contract is required (use contract = 1)"))
	case *r.Contract != app.ContractVersion:
		errs = append(errs, fmt.Errorf("contract = %d is not supported by this dootd (supported: %d)", *r.Contract, app.ContractVersion))
	default:
		m.Contract = *r.Contract
	}

	switch {
	case r.ZigVersion == nil:
		errs = append(errs, errors.New(`zig_version is required, e.g. zig_version = "0.16.0"`))
	case !zigVersionRe.MatchString(*r.ZigVersion):
		errs = append(errs, fmt.Errorf("zig_version %q must be an exact release like \"0.16.0\" (not master or a range)", *r.ZigVersion))
	default:
		m.ZigVersion = *r.ZigVersion
	}

	if r.Build != nil {
		if strings.TrimSpace(*r.Build) == "" {
			errs = append(errs, errors.New("build must not be empty (omit it to use the default)"))
		} else {
			m.Build = *r.Build
		}
	}

	if r.Run == nil {
		errs = append(errs, errors.New(`run is required, e.g. run = "zig-out/bin/myapp"`))
	} else if argv, err := app.SplitCommand(*r.Run); err != nil {
		errs = append(errs, fmt.Errorf("run: %w", err))
	} else if err := checkRelPath(argv[0]); err != nil {
		errs = append(errs, fmt.Errorf("run: %w", err))
	} else {
		m.Run = argv
	}

	if r.HealthPath != nil {
		if !strings.HasPrefix(*r.HealthPath, "/") || strings.ContainsAny(*r.HealthPath, " \t\r\n") {
			errs = append(errs, fmt.Errorf("health_path %q must start with '/' and contain no spaces", *r.HealthPath))
		} else {
			m.HealthPath = *r.HealthPath
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid %s:\n%w", FileName, err)
	}
	return m, nil
}

// checkRelPath requires a clean path relative to the app root.
func checkRelPath(p string) error {
	if filepath.IsAbs(p) {
		return fmt.Errorf("binary path %q must be relative to the app root", p)
	}
	if c := filepath.Clean(p); c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("binary path %q must stay inside the app root", p)
	}
	return nil
}
