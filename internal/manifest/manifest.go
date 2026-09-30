// Package manifest parses and validates an app's dootd.toml
// (docs/app-contract.md §2).
package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sumitwaani2/dootd/internal/app"
)

// FileName is the manifest file name at the app root.
const FileName = "dootd.toml"

const maxSize = 64 << 10

// Manifest is a validated dootd.toml (contract 2: it only says how to run
// the app; the app is built by its release workflow).
type Manifest struct {
	Contract   int
	Run        []string // argv; Run[0] relative to the release root
	HealthPath string
}

type raw struct {
	Contract   *int    `toml:"contract"`
	Run        *string `toml:"run"`
	HealthPath *string `toml:"health_path"`
}

// Load reads dir/dootd.toml and validates it.
func Load(dir string) (*Manifest, error) {
	p := filepath.Join(dir, FileName)
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s not found at the root of the release tarball; see docs/app-contract.md", FileName)
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
	return Parse(b)
}

// Parse validates manifest content, reporting every problem at once.
func Parse(b []byte) (*Manifest, error) {
	var r raw
	md, err := toml.Decode(string(b), &r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	var errs []error
	for _, k := range md.Undecoded() {
		switch k.String() {
		case "zig_version", "build":
			errs = append(errs, fmt.Errorf("%s is not used any more: the app is built by its release workflow (docs/app-contract.md §2); remove it", k))
		default:
			errs = append(errs, fmt.Errorf("unknown key %q", k.String()))
		}
	}
	m := &Manifest{HealthPath: app.DefaultHealthPath}

	switch {
	case r.Contract == nil:
		errs = append(errs, fmt.Errorf("contract is required (use contract = %d)", app.ContractVersion))
	case *r.Contract == 1:
		errs = append(errs, errors.New("contract = 1 (dootd builds the app) is no longer supported: build it in GitHub Actions and use contract = 2 (docs/app-contract.md §2)"))
	case *r.Contract != app.ContractVersion:
		errs = append(errs, fmt.Errorf("contract = %d is not supported by this dootd (supported: %d)", *r.Contract, app.ContractVersion))
	default:
		m.Contract = *r.Contract
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
		return fmt.Errorf("binary path %q must be relative to the tarball root", p)
	}
	if c := filepath.Clean(p); c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("binary path %q must stay inside the release", p)
	}
	return nil
}
