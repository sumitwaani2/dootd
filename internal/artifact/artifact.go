// Package artifact checks and unpacks release tarballs (docs/architecture.md
// §11): the SHA-256 from checksums.txt, safe extraction, and an ELF check
// that the app binary runs on this machine.
package artifact

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxUnpacked bounds the unpacked size of a release.
const MaxUnpacked = 1 << 30

// MaxEntries bounds the number of entries in a release tarball.
const MaxEntries = 20000

// AssetName is the tarball asset for a Go architecture (amd64, arm64).
func AssetName(goarch string) string { return "app-linux-" + goarch + ".tar.gz" }

// ChecksumsName is the checksums asset.
const ChecksumsName = "checksums.txt"

var sumLine = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](.+)$`)

// Checksum returns the SHA-256 listed for name in a sha256sum-style file.
func Checksum(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		m := sumLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m != nil && (m[2] == name || m[2] == "./"+name) {
			return strings.ToLower(m[1]), nil
		}
	}
	return "", fmt.Errorf("%s has no line for %s", ChecksumsName, name)
}

// FileSHA256 hashes a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Unpack extracts the gzipped tarball archive into dst (created). It
// rejects absolute paths, "..", device files, hard links, symlinks that
// point outside dst, paths through symlinks, and archives that expand to
// more than max bytes. Files get mode 0644, or 0755 if any exec bit is set.
func Unpack(archive, dst string, max int64) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip file: %w", err)
	}
	defer zr.Close()
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for n := 0; ; n++ {
		if n > MaxEntries {
			return fmt.Errorf("archive has more than %d entries", MaxEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || name == "." || name == "./" {
			continue
		}
		rel := filepath.Clean(name)
		if filepath.IsAbs(name) || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("unsafe path %q in archive", h.Name)
		}
		target := filepath.Join(dst, rel)
		// A path through a symlink extracted earlier could escape dst
		// (a/b -> .., a/b/c -> .. resolves outside), so parents must be
		// real directories and files must not exist yet.
		if err := realParents(dst, rel); err != nil {
			return fmt.Errorf("unsafe path %q in archive: %w", h.Name, err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if st, err := os.Lstat(target); err == nil && !st.IsDir() {
				return fmt.Errorf("unsafe path %q in archive: exists and is not a directory", h.Name)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > max {
				return fmt.Errorf("archive expands to more than %d MB", max>>20)
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

// realParents checks that every existing parent of rel below dst is a
// directory, not a symlink.
func realParents(dst, rel string) error {
	cur := dst
	for _, p := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if p == "." || p == "" {
			continue
		}
		cur = filepath.Join(cur, p)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", strings.TrimPrefix(cur, dst+"/"))
		}
	}
	return nil
}

var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// CheckBinary checks that rel (relative to root, staying inside it after
// resolving symlinks) is an executable Linux ELF binary for goarch.
func CheckBinary(root, rel, goarch string) error {
	p := filepath.Join(root, rel)
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("run: %s is not in the release tarball (add it to FILES in the release workflow)", rel)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	if real != rootReal && !strings.HasPrefix(real, rootReal+"/") {
		return fmt.Errorf("run: %s points outside the release", rel)
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("run: %s is not a regular file", rel)
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("run: %s is not executable", rel)
	}
	f, err := elf.Open(real)
	if err != nil {
		return fmt.Errorf("run: %s is not a Linux (ELF) binary", rel)
	}
	defer f.Close()
	want, ok := machines[goarch]
	if !ok {
		return fmt.Errorf("unsupported server architecture %s", goarch)
	}
	if f.Machine != want {
		built := f.Machine.String()
		for arch, m := range machines {
			if m == f.Machine {
				built = arch
			}
		}
		return fmt.Errorf("run: %s is built for %s, but this server is %s; the release has the wrong binary in %s", rel, built, goarch, AssetName(goarch))
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("run: %s is not an executable (ELF type %s)", rel, f.Type)
	}
	return nil
}
