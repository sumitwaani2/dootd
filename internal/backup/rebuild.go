package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/sumitwaani2/dootd/internal/s3"
)

// Helpers for rebuilding a server from the bucket (`dootd init --restore`),
// used without a running Service.

// KeyPrefix is where the backups of app live in the bucket.
func KeyPrefix(prefix, hostID, app string) string { return prefix + "/" + hostID + "/" + app + "/" }

// Newest returns the newest backup object by the time in its name, skipping
// anything that is not a dootd archive.
func Newest(objs []s3.Object) (s3.Object, bool) {
	var ok []s3.Object
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".zst") {
			ok = append(ok, o)
		}
	}
	if len(ok) == 0 {
		return s3.Object{}, false
	}
	sort.Slice(ok, func(i, j int) bool { return objTime(ok[i]).After(objTime(ok[j])) })
	return ok[0], true
}

// Decompress writes the zstd-decompressed src (a dootd.db backup) to dst.
func Decompress(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := zstd.NewReader(in, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(zr, maxRestoreBytes))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// UnpackInto verifies archive (manifest checksums, quick_check) in a
// temporary folder under staging and only then copies the databases into
// dataDir owned by uid:gid. dataDir must not contain those databases yet.
func UnpackInto(ctx context.Context, archive, staging, dataDir, app string, uid, gid int) ([]string, error) {
	work, err := os.MkdirTemp(staging, "unpack-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	m, err := extractArchive(archive, work)
	if err != nil {
		return nil, err
	}
	if m.App != app {
		return nil, fmt.Errorf("archive contains %s, not %s", m.App, app)
	}
	var files []string
	for _, f := range m.Files {
		if err := QuickCheck(ctx, filepath.Join(work, f.Path)); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		files = append(files, f.Path)
	}
	for _, f := range m.Files {
		dst := filepath.Join(dataDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		for d := filepath.Dir(dst); d != dataDir && strings.HasPrefix(d, dataDir); d = filepath.Dir(d) {
			os.Chown(d, uid, gid)
		}
		if err := copyFile(filepath.Join(work, f.Path), dst); err != nil {
			return nil, err
		}
		if err := os.Chown(dst, uid, gid); err != nil {
			return nil, err
		}
	}
	return files, nil
}
