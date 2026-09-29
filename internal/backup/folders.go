package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sumitwaani2/dootd/internal/s3"
)

// Folders lists the backup folders in the bucket (one per app name), for
// the Add app form (docs/architecture.md §12.1). ok is false without a bucket.
func (s *Service) Folders(ctx context.Context) (folders []string, ok bool, err error) {
	cl, _, ok, err := s.client(ctx)
	if err != nil || !ok {
		return nil, ok, err
	}
	all, err := cl.Folders(ctx)
	if err != nil {
		return nil, true, err
	}
	for _, f := range all {
		if !strings.HasPrefix(f, ".") {
			folders = append(folders, f)
		}
	}
	return folders, true, nil
}

// RestoreFolder puts the newest backup in the bucket folder into the empty
// DATA_DIR of a new app, after verifying it completely (manifest checksums
// and quick_check). The folder may belong to another name (a renamed repo).
func (s *Service) RestoreFolder(ctx context.Context, app, folder string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cl, _, ok, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("no bucket is configured")
	}
	if strings.ContainsAny(folder, "/\\") || folder == "" || strings.HasPrefix(folder, ".") {
		return nil, fmt.Errorf("invalid backup folder %q", folder)
	}
	objs, err := cl.List(ctx, Folder(folder))
	if err != nil {
		return nil, err
	}
	o, ok := Newest(objs)
	if !ok {
		return nil, fmt.Errorf("the folder %s/ has no backups", folder)
	}
	dataDir := s.Layout.DataDir(app)
	if dbs, err := FindDatabases(dataDir); err != nil {
		return nil, err
	} else if len(dbs) > 0 {
		return nil, fmt.Errorf("%s already has databases in DATA_DIR; use Restore on the app page instead", app)
	}
	uid, gid, err := s.Hooks.Owner(app)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(s.stagingDir(), "folder-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	archive := filepath.Join(work, "download.tar.zst")
	if err := cl.GetFile(ctx, o.Key, archive); err != nil {
		return nil, err
	}
	files, err := UnpackInto(ctx, archive, work, dataDir, folder, uid, gid)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Key, err)
	}
	s.Log.Info("restored a bucket folder into a new app", "app", app, "folder", folder, "key", o.Key, "files", files)
	return files, nil
}

// Newest returns the newest backup object by the time in its name, skipping
// anything that is not a dootd archive.
func Newest(objs []s3.Object) (s3.Object, bool) {
	var ok []s3.Object
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".tar.zst") {
			ok = append(ok, o)
		}
	}
	if len(ok) == 0 {
		return s3.Object{}, false
	}
	sort.Slice(ok, func(i, j int) bool { return objTime(ok[i]).After(objTime(ok[j])) })
	return ok[0], true
}

// UnpackInto verifies archive (manifest checksums, quick_check) in a
// temporary folder under staging and only then copies the databases into
// dataDir owned by uid:gid. The manifest must name wantApp.
func UnpackInto(ctx context.Context, archive, staging, dataDir, wantApp string, uid, gid int) ([]string, error) {
	work, err := os.MkdirTemp(staging, "unpack-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	m, err := extractArchive(archive, work)
	if err != nil {
		return nil, err
	}
	if m.App != wantApp {
		return nil, fmt.Errorf("archive contains %s, not %s", m.App, wantApp)
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
