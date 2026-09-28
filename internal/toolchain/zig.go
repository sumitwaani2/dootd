// Package toolchain downloads, verifies and caches pinned Zig releases
// (docs/architecture.md §11.1).
//
// Layout: <dir>/<version>/zig (plus lib/ etc.), root-owned and world
// readable. Installs are atomic: a version directory either exists
// complete or not at all.
package toolchain

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"
)

// DefaultIndexURL is the official Zig release index.
const DefaultIndexURL = "https://ziglang.org/download/index.json"

const (
	indexTTL        = 6 * time.Hour
	maxIndexSize    = 16 << 20
	maxTarballSize  = 512 << 20
	maxExtractBytes = 4 << 30
)

var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// ErrUnknownVersion means the version is not in the Zig release index.
var ErrUnknownVersion = errors.New("zig version not found in the official release index")

// Zig manages installed Zig toolchains.
type Zig struct {
	Dir      string // e.g. /var/lib/dootd/toolchains/zig
	IndexURL string
	HTTP     *http.Client
	Arch     string // GOARCH; defaults to runtime.GOARCH

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	index   map[string]map[string]json.RawMessage
	fetched time.Time
}

// Installed is one cached toolchain.
type Installed struct {
	Version string
	Path    string
	Bytes   int64
}

type entry struct {
	Tarball string `json:"tarball"`
	Shasum  string `json:"shasum"`
	Size    string `json:"size"`
}

// New returns a manager rooted at dir.
func New(dir string) *Zig {
	return &Zig{Dir: dir, IndexURL: DefaultIndexURL, HTTP: &http.Client{Timeout: 15 * time.Minute}}
}

// BinDir returns the directory containing the zig binary for version.
func (z *Zig) BinDir(version string) string { return filepath.Join(z.Dir, version) }

func (z *Zig) lock(version string) *sync.Mutex {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.locks == nil {
		z.locks = map[string]*sync.Mutex{}
	}
	if z.locks[version] == nil {
		z.locks[version] = &sync.Mutex{}
	}
	return z.locks[version]
}

// Ensure makes sure version is installed, downloading it if needed, and
// returns its directory. progress receives human-readable status lines.
func (z *Zig) Ensure(ctx context.Context, version string, progress func(string)) (string, error) {
	if !versionRe.MatchString(version) {
		return "", fmt.Errorf("invalid zig version %q", version)
	}
	if progress == nil {
		progress = func(string) {}
	}
	l := z.lock(version)
	l.Lock()
	defer l.Unlock()

	dir := z.BinDir(version)
	if st, err := os.Stat(filepath.Join(dir, "zig")); err == nil && st.Mode().IsRegular() {
		progress(fmt.Sprintf("zig %s already installed", version))
		return dir, nil
	}

	e, err := z.lookup(ctx, version)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(z.Dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(z.Dir, ".tmp-"+version+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	progress(fmt.Sprintf("downloading zig %s (%s MB) from %s", version, mb(e.Size), e.Tarball))
	archive := filepath.Join(tmp, "zig.tar.xz")
	if err := z.download(ctx, e, archive); err != nil {
		return "", err
	}
	progress("checksum verified; extracting")
	out := filepath.Join(tmp, "root")
	if err := extractTarXz(archive, out); err != nil {
		return "", fmt.Errorf("extract zig %s: %w", version, err)
	}
	os.Remove(archive)

	// Sanity check: the binary runs and reports the expected version.
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	got, err := exec.CommandContext(vctx, filepath.Join(out, "zig"), "version").Output()
	if err != nil {
		return "", fmt.Errorf("zig %s: downloaded binary does not run: %w", version, err)
	}
	if v := strings.TrimSpace(string(got)); v != version {
		return "", fmt.Errorf("zig %s: downloaded binary reports version %q", version, v)
	}

	if err := os.Rename(out, dir); err != nil {
		return "", fmt.Errorf("install zig %s: %w", version, err)
	}
	progress(fmt.Sprintf("zig %s installed", version))
	return dir, nil
}

func mb(size string) string {
	var n int64
	fmt.Sscan(size, &n)
	return fmt.Sprintf("%.1f", float64(n)/(1<<20))
}

func (z *Zig) archKey() (string, error) {
	a := z.Arch
	if a == "" {
		a = runtime.GOARCH
	}
	switch a {
	case "amd64":
		return "x86_64-linux", nil
	case "arm64":
		return "aarch64-linux", nil
	}
	return "", fmt.Errorf("unsupported architecture %q", a)
}

func (z *Zig) lookup(ctx context.Context, version string) (entry, error) {
	idx, err := z.getIndex(ctx, false)
	if err != nil {
		return entry{}, err
	}
	if _, ok := idx[version]; !ok {
		// The cached index may predate a brand-new release.
		if idx, err = z.getIndex(ctx, true); err != nil {
			return entry{}, err
		}
	}
	rel, ok := idx[version]
	if !ok {
		return entry{}, fmt.Errorf("%w: %s (see https://ziglang.org/download/)", ErrUnknownVersion, version)
	}
	key, err := z.archKey()
	if err != nil {
		return entry{}, err
	}
	rawE, ok := rel[key]
	if !ok {
		return entry{}, fmt.Errorf("zig %s has no %s build", version, key)
	}
	var e entry
	if err := json.Unmarshal(rawE, &e); err != nil {
		return entry{}, fmt.Errorf("zig index: bad entry for %s/%s: %w", version, key, err)
	}
	if len(e.Shasum) != 64 || !strings.HasPrefix(e.Tarball, "https://") {
		return entry{}, fmt.Errorf("zig index: entry for %s/%s lacks an https tarball or sha256", version, key)
	}
	return e, nil
}

func (z *Zig) getIndex(ctx context.Context, force bool) (map[string]map[string]json.RawMessage, error) {
	z.mu.Lock()
	if !force && z.index != nil && time.Since(z.fetched) < indexTTL {
		idx := z.index
		z.mu.Unlock()
		return idx, nil
	}
	z.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, z.IndexURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := z.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch zig index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch zig index: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexSize))
	if err != nil {
		return nil, fmt.Errorf("fetch zig index: %w", err)
	}
	// Top level: version -> {key -> value}. Some values (date, docs) are
	// strings, so keep them raw and decode entries on demand.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("parse zig index: %w", err)
	}
	idx := map[string]map[string]json.RawMessage{}
	for v, rel := range top {
		var m map[string]json.RawMessage
		if json.Unmarshal(rel, &m) == nil {
			idx[v] = m
		}
	}
	z.mu.Lock()
	z.index, z.fetched = idx, time.Now()
	z.mu.Unlock()
	return idx, nil
}

func (z *Zig) download(ctx context.Context, e entry, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.Tarball, nil)
	if err != nil {
		return err
	}
	resp, err := z.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", e.Tarball, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", e.Tarball, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxTarballSize+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", e.Tarball, err)
	}
	if n > maxTarballSize {
		return fmt.Errorf("download %s: larger than %d MB", e.Tarball, maxTarballSize>>20)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, e.Shasum) {
		return fmt.Errorf("download %s: sha256 mismatch (expected %s, got %s)", e.Tarball, e.Shasum, got)
	}
	return f.Close()
}

// extractTarXz extracts archive into dst (created), stripping the single
// top-level directory. It rejects absolute paths, "..", device files and
// symlinks that point outside dst.
func extractTarXz(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(xr)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		_, rest, _ := strings.Cut(name, "/") // strip top-level dir
		if rest == "" {
			continue
		}
		rel := filepath.Clean(rest)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("unsafe path %q in archive", h.Name)
		}
		target := filepath.Join(dst, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxExtractBytes {
				return errors.New("archive expands to more than 4 GB")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(out, tr, h.Size); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			lt := h.Linkname
			resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), lt))
			if filepath.IsAbs(lt) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink %q -> %q escapes the archive", h.Name, lt)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(lt, target); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// metadata only
		default:
			return fmt.Errorf("unsupported entry type %q for %q", h.Typeflag, h.Name)
		}
	}
}

// List returns installed toolchains, newest version first.
func (z *Zig) List() ([]Installed, error) {
	entries, err := os.ReadDir(z.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Installed
	for _, e := range entries {
		if !e.IsDir() || !versionRe.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(z.Dir, e.Name())
		var size int64
		filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, err := d.Info(); err == nil {
					size += fi.Size()
				}
			}
			return nil
		})
		out = append(out, Installed{Version: e.Name(), Path: p, Bytes: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

// Delete removes an installed version. Callers must check it is unused.
func (z *Zig) Delete(version string) error {
	if !versionRe.MatchString(version) {
		return fmt.Errorf("invalid zig version %q", version)
	}
	l := z.lock(version)
	l.Lock()
	defer l.Unlock()
	dir := z.BinDir(version)
	// Rename first so a half-deleted toolchain is never used.
	trash := filepath.Join(z.Dir, ".trash-"+version+"-"+randHex())
	if err := os.Rename(dir, trash); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.RemoveAll(trash)
}

// CleanTemp removes leftovers of interrupted installs or deletes.
func (z *Zig) CleanTemp() {
	entries, _ := os.ReadDir(z.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") || strings.HasPrefix(e.Name(), ".trash-") {
			os.RemoveAll(filepath.Join(z.Dir, e.Name()))
		}
	}
}

func randHex() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
