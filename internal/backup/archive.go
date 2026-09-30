package backup

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ManifestVersion is the archive format version.
const ManifestVersion = 1

// Manifest is manifest.json, the first entry of every archive.
type Manifest struct {
	Version      int            `json:"version"`
	App          string         `json:"app"`
	Kind         string         `json:"kind"`
	CreatedAt    time.Time      `json:"created_at"`
	Release      string         `json:"release"`
	DootdVersion string         `json:"dootd_version"`
	Files        []ManifestFile `json:"files"`
}

// ManifestFile is one database in the archive.
type ManifestFile struct {
	Path   string `json:"path"` // relative to DATA_DIR
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// writeArchive writes manifest.json and the snapshot files (from dir,
// named by their manifest path) as tar+zstd to out. It fills sizes and
// hashes in m and returns the archive's SHA-256 and size.
func writeArchive(out string, m *Manifest, dir string) (string, int64, error) {
	for i, f := range m.Files {
		sum, n, err := fileSHA256(filepath.Join(dir, f.Path))
		if err != nil {
			return "", 0, err
		}
		m.Files[i].SHA256, m.Files[i].Size = sum, n
	}
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", 0, err
	}
	fh, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	zw, err := zstd.NewWriter(io.MultiWriter(fh, h), zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		fh.Close()
		return "", 0, err
	}
	tw := tar.NewWriter(zw)
	fail := func(err error) (string, int64, error) {
		tw.Close()
		zw.Close()
		fh.Close()
		os.Remove(out)
		return "", 0, err
	}
	now := time.Now()
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mj)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
		return fail(err)
	}
	if _, err := tw.Write(mj); err != nil {
		return fail(err)
	}
	for _, f := range m.Files {
		src, err := os.Open(filepath.Join(dir, f.Path))
		if err != nil {
			return fail(err)
		}
		err = tw.WriteHeader(&tar.Header{Name: "data/" + filepath.ToSlash(f.Path), Mode: 0o600, Size: f.Size, ModTime: now, Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.CopyN(tw, src, f.Size)
		}
		src.Close()
		if err != nil {
			return fail(err)
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := fh.Sync(); err != nil {
		return fail(err)
	}
	st, err := fh.Stat()
	if err != nil {
		return fail(err)
	}
	if err := fh.Close(); err != nil {
		return fail(err)
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

const maxRestoreBytes = 32 << 30

// extractArchive unpacks an archive into dir (which must exist and be
// empty), verifies every file against the manifest and returns it.
func extractArchive(archive, dir string) (*Manifest, error) {
	fh, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	zr, err := zstd.NewReader(fh, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	h, err := tr.Next()
	if err != nil || h.Name != "manifest.json" || h.Size > 1<<20 {
		return nil, errors.New("not a dootd backup archive (manifest.json must come first)")
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("unsupported backup format version %d", m.Version)
	}
	want := map[string]ManifestFile{}
	for _, f := range m.Files {
		if err := safeRel(f.Path); err != nil {
			return nil, err
		}
		want["data/"+filepath.ToSlash(f.Path)] = f
	}
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		f, ok := want[h.Name]
		if !ok || h.Typeflag != tar.TypeReg || seen[h.Name] {
			return nil, fmt.Errorf("unexpected entry %q in archive", h.Name)
		}
		seen[h.Name] = true
		total += h.Size
		if h.Size != f.Size || total > maxRestoreBytes {
			return nil, fmt.Errorf("%s: size does not match the manifest", f.Path)
		}
		dst := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		hs := sha256.New()
		_, err = io.CopyN(io.MultiWriter(out, hs), tr, h.Size)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		if hex.EncodeToString(hs.Sum(nil)) != f.SHA256 {
			return nil, fmt.Errorf("%s: checksum does not match the manifest", f.Path)
		}
	}
	if len(seen) != len(want) {
		return nil, errors.New("archive is missing files listed in its manifest")
	}
	return &m, nil
}

func safeRel(p string) error {
	c := filepath.Clean(p)
	if p == "" || filepath.IsAbs(p) || c == ".." || strings.HasPrefix(c, "../") || c != p {
		return fmt.Errorf("unsafe path %q in manifest", p)
	}
	return nil
}
