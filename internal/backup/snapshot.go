// Package backup takes consistent snapshots of the SQLite databases in each
// app's DATA_DIR, archives them (tar + zstd, with a manifest of SHA-256
// sums), keeps the newest copies locally, uploads them to S3-compatible
// storage, applies retention and restores them (docs/architecture.md §12).
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	_ "modernc.org/sqlite"
)

var sqliteHeader = []byte("SQLite format 3\x00")

// skipDir reports directories never included in backups.
func skipDir(name string) bool { return strings.HasPrefix(name, ".pre-restore-") }

// sidecar reports SQLite's companion files.
func sidecar(name string) bool {
	for _, s := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// FindDatabases returns the SQLite database files under dir (relative
// paths, sorted), detected by their header rather than their name.
func FindDatabases(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || sidecar(d.Name()) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		hdr := make([]byte, len(sqliteHeader))
		n, _ := io.ReadFull(f, hdr)
		f.Close()
		if n == len(hdr) && bytes.Equal(hdr, sqliteHeader) {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

func dsn(path string, extra ...string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	for _, e := range extra {
		q.Add("_pragma", e)
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

// Snapshot writes a consistent copy of the live database src to dst with
// VACUUM INTO (safe while the app writes; works in WAL mode) and checks
// the copy with quick_check. Companion -wal/-shm files that SQLite may
// create while dootd (root) has the database open are given back to the
// database's owner, so the app can still open it.
func Snapshot(ctx context.Context, src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	owner, _ := st.Sys().(*syscall.Stat_t)
	defer func() {
		if owner == nil {
			return
		}
		for _, s := range []string{"-wal", "-shm"} {
			if fi, err := os.Stat(src + s); err == nil {
				if o, ok := fi.Sys().(*syscall.Stat_t); ok && (o.Uid != owner.Uid || o.Gid != owner.Gid) {
					os.Chown(src+s, int(owner.Uid), int(owner.Gid))
				}
			}
		}
	}()

	db, err := sql.Open("sqlite", dsn(src))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	cerr := db.Close()
	if err != nil {
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	if cerr != nil {
		return cerr
	}
	return QuickCheck(ctx, dst)
}

// QuickCheck runs PRAGMA quick_check on a standalone database file.
func QuickCheck(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", dsn(path, "query_only(1)"))
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("quick_check: %s", res)
	}
	return nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
