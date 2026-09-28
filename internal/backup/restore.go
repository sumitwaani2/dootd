package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/internal/logs"
)

// KeepPreRestore is how many .pre-restore-* folders are kept per app.
const KeepPreRestore = 2

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Files      []string // restored databases
	MovedTo    string   // where the previous databases were moved
	Restarted  bool
	StartError string
}

// Restore replaces an app's databases with backup id (Req 15):
// fetch and verify the archive, and only then stop the app, move the
// current databases (with -wal/-shm) into DATA_DIR/.pre-restore-<time>/,
// put the restored files in place owned by the app user, and start the
// app again if it was running. Verification failures leave the app alone.
func (s *Service) Restore(ctx context.Context, app string, id int64, lg *logs.Log) (RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res RestoreResult
	b, err := s.Get(ctx, id)
	if err != nil {
		return res, err
	}
	if b.App != app {
		return res, fmt.Errorf("backup #%d belongs to %s, not %s", id, b.App, app)
	}
	if !b.Restorable() {
		return res, fmt.Errorf("backup #%d cannot be restored (status %s, no copy available)", id, b.Status)
	}
	release, err := s.Hooks.Reserve(app)
	if err != nil {
		return res, err
	}
	defer release()

	// 1. Fetch and verify, while the app keeps running.
	work, err := os.MkdirTemp(s.stagingDir(), "restore-"+app+"-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(work)
	archive, err := s.fetch(ctx, b, work)
	if err != nil {
		return res, err
	}
	out := filepath.Join(work, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return res, err
	}
	m, err := extractArchive(archive, out)
	if err != nil {
		return res, fmt.Errorf("backup #%d failed verification: %w", id, err)
	}
	if m.App != app {
		return res, fmt.Errorf("backup #%d contains %s, not %s", id, m.App, app)
	}
	for _, f := range m.Files {
		if err := QuickCheck(ctx, filepath.Join(out, f.Path)); err != nil {
			return res, fmt.Errorf("backup #%d: %s: %w", id, f.Path, err)
		}
		res.Files = append(res.Files, f.Path)
	}
	note(lg, "backup #%d verified: %d database(s)", id, len(m.Files))
	uid, gid, err := s.Hooks.Owner(app)
	if err != nil {
		return res, err
	}

	// 2. Safety copy of the current state, taken while it is consistent.
	dataDir := s.Layout.DataDir(app)
	wasRunning, err := s.Hooks.Stop(ctx, app)
	if err != nil {
		return res, fmt.Errorf("stop %s: %w", app, err)
	}
	start := func() {
		if !wasRunning {
			return
		}
		if err := s.Hooks.Start(context.WithoutCancel(ctx), app); err != nil {
			res.StartError = err.Error()
			return
		}
		res.Restarted = true
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")
	pre := filepath.Join(dataDir, ".pre-restore-"+stamp)
	current, err := FindDatabases(dataDir)
	if err != nil {
		start()
		return res, err
	}
	moved := false
	for _, rel := range current {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			src := filepath.Join(dataDir, rel+suffix)
			if _, err := os.Lstat(src); err != nil {
				continue
			}
			dst := filepath.Join(pre, rel+suffix)
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				start()
				return res, err
			}
			if err := os.Rename(src, dst); err != nil {
				start()
				return res, fmt.Errorf("move %s aside: %w", rel+suffix, err)
			}
			moved = true
		}
	}
	// Restored files may not exist now, but their stale sidecars might.
	for _, f := range m.Files {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			src := filepath.Join(dataDir, f.Path+suffix)
			if _, err := os.Lstat(src); err == nil {
				dst := filepath.Join(pre, f.Path+suffix)
				os.MkdirAll(filepath.Dir(dst), 0o700)
				os.Rename(src, dst)
				moved = true
			}
		}
	}
	if moved {
		chownTree(pre, uid, gid)
		os.Chmod(pre, 0o700)
		res.MovedTo = pre
		note(lg, "previous databases moved to %s", pre)
	}

	// 3. Put the restored files in place.
	for _, f := range m.Files {
		dst := filepath.Join(dataDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			start()
			return res, err
		}
		if err := copyFile(filepath.Join(out, f.Path), dst); err != nil {
			start()
			return res, fmt.Errorf("restore %s: %w", f.Path, err)
		}
		for d := filepath.Dir(dst); d != dataDir && strings.HasPrefix(d, dataDir); d = filepath.Dir(d) {
			os.Chown(d, uid, gid)
		}
		if err := os.Chown(dst, uid, gid); err != nil {
			start()
			return res, err
		}
	}
	s.prunePreRestore(dataDir)
	s.Log.Info("backup restored", "app", app, "id", id, "files", len(m.Files), "moved_to", pre)
	note(lg, "restored %s", strings.Join(res.Files, ", "))
	start()
	return res, nil
}

// fetch returns a verified local archive: the local copy if it is intact,
// otherwise a download from the bucket.
func (s *Service) fetch(ctx context.Context, b Backup, work string) (string, error) {
	if b.LocalPath != "" {
		if sum, _, err := fileSHA256(b.LocalPath); err == nil && sum == b.SHA256 {
			return b.LocalPath, nil
		}
	}
	if b.ObjectKey == "" {
		return "", fmt.Errorf("backup #%d: the local copy is missing or damaged and it was never uploaded", b.ID)
	}
	cl, _, ok, err := s.client(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("the local copy is gone and no S3 bucket is configured")
	}
	dst := filepath.Join(work, "download.tar.zst")
	if err := cl.GetFile(ctx, b.ObjectKey, dst); err != nil {
		return "", err
	}
	sum, _, err := fileSHA256(dst)
	if err != nil {
		return "", err
	}
	if b.SHA256 != "" && sum != b.SHA256 {
		return "", fmt.Errorf("backup #%d: downloaded archive checksum does not match (expected %s, got %s)", b.ID, b.SHA256, sum)
	}
	return dst, nil
}

func (s *Service) prunePreRestore(dataDir string) {
	entries, _ := os.ReadDir(dataDir)
	var names []string
	for _, e := range entries {
		if e.IsDir() && skipDir(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names[min(len(names), KeepPreRestore):] {
		os.RemoveAll(filepath.Join(dataDir, n))
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".dootd-restore"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func chownTree(dir string, uid, gid int) {
	filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err == nil {
			os.Lchown(p, uid, gid)
		}
		return nil
	})
}
