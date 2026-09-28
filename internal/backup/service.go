package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sumitwaani2/dootd/internal/buildinfo"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/s3"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
)

// Kinds.
const (
	KindScheduled  = "scheduled"
	KindPreDeploy  = "pre-deploy"
	KindManual     = "manual"
	KindPreRestore = "pre-restore"
)

// Statuses.
const (
	StatusRunning = "running"
	StatusOK      = "ok"
	StatusFailed  = "failed"
)

// SelfApp is the pseudo app name for dootd.db backups.
const SelfApp = "_dootd"

// Default policy (docs/architecture.md §12); see Service.Interval/Retention.
const (
	DefaultInterval  = 3 * time.Hour
	DefaultRetention = 48 * time.Hour
	SelfInterval     = 24 * time.Hour
	SelfRetention    = 7 * 24 * time.Hour
	KeepLocal        = 2
	uploadAttempts   = 3
)

// Settings keys.
const (
	SettingS3        = "s3_config"
	purposeS3        = "settings:s3"
	SettingHostID    = "host_id"
	SettingKitSaved  = "recovery_kit_downloaded_at"
	settingSelfLast  = "dootd_db_backup_at"
	uploadRetryDelay = 3 * time.Second
	uploadPending    = "upload pending"
)

// ErrNoDatabases means DATA_DIR has no SQLite files, so nothing to back up.
var ErrNoDatabases = errors.New("no SQLite databases in DATA_DIR")

// Backup is one row of the backups table.
type Backup struct {
	ID         int64     `json:"id"`
	App        string    `json:"app"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	ObjectKey  string    `json:"object_key"`
	LocalPath  string    `json:"local_path"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	Files      int       `json:"files"`
	ReleaseID  string    `json:"release_id"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Uploaded reports whether the archive is in the bucket.
func (b Backup) Uploaded() bool { return b.ObjectKey != "" }

// Restorable reports whether a copy is available.
func (b Backup) Restorable() bool {
	return b.Status == StatusOK && b.App != SelfApp && (b.ObjectKey != "" || b.LocalPath != "")
}

// Hooks are the app operations the service needs (implemented in serve).
type Hooks struct {
	// Apps lists the apps to back up on schedule.
	Apps func() []string
	// Release returns the app's current release id ("" if none).
	Release func(app string) string
	// Reserve blocks deployments of app during a restore; it fails if a
	// deployment is in progress.
	Reserve func(app string) (release func(), err error)
	// Stop stops the app and reports whether it was running.
	Stop func(ctx context.Context, app string) (wasRunning bool, err error)
	// Start starts the app again.
	Start func(ctx context.Context, app string) error
	// Owner returns the app user's uid/gid.
	Owner func(app string) (uid, gid int, err error)
}

// Service runs backups and restores.
type Service struct {
	Layout layout.Layout
	Store  *store.Store
	Box    *secrets.Box
	Log    *slog.Logger
	Hooks  Hooks
	// SelfSnapshot writes a consistent copy of dootd.db to the given path.
	SelfSnapshot func(ctx context.Context, dst string) error
	// Interval between scheduled backups and how long they are kept
	// (zero = defaults).
	Interval  time.Duration
	Retention time.Duration

	mu      sync.Mutex // one backup/restore at a time
	uploads sync.WaitGroup
	hostID  string
}

func (s *Service) stagingDir() string { return filepath.Join(s.Layout.Root, "backups", "staging") }
func (s *Service) localDir(app string) string {
	return filepath.Join(s.Layout.Root, "backups", "local", app)
}

// Init prepares directories, the host id, and marks interrupted backups.
func (s *Service) Init(ctx context.Context) error {
	for _, d := range []string{filepath.Join(s.Layout.Root, "backups"), s.stagingDir(), filepath.Join(s.Layout.Root, "backups", "local")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	entries, _ := os.ReadDir(s.stagingDir())
	for _, e := range entries {
		os.RemoveAll(filepath.Join(s.stagingDir(), e.Name()))
	}
	v, ok, err := s.Store.GetSetting(ctx, SettingHostID)
	if err != nil {
		return err
	}
	if !ok {
		b := make([]byte, 6)
		rand.Read(b)
		v = []byte(hex.EncodeToString(b))
		if err := s.Store.SetSetting(ctx, SettingHostID, v); err != nil {
			return err
		}
	}
	s.hostID = string(v)
	_, err = s.Store.Writer().ExecContext(ctx, `UPDATE backups SET status = ?, error = 'interrupted: dootd restarted', finished_at = ?
		WHERE status = ?`, StatusFailed, time.Now().Unix(), StatusRunning)
	return err
}

// UploadPending uploads recent backups that exist only locally: uploads
// interrupted by a restart, failed uploads, and backups taken before a
// bucket was configured.
func (s *Service) UploadPending(ctx context.Context) {
	if _, ok, err := s.S3Config(ctx); err != nil || !ok {
		return
	}
	rows, err := s.Store.Reader().QueryContext(ctx, `SELECT `+cols+` FROM backups WHERE status = ? AND object_key = ''
		AND local_path != '' AND created_at > ? ORDER BY id`, StatusOK, time.Now().Add(-s.retention()).Unix())
	if err != nil {
		return
	}
	var todo []Backup
	for rows.Next() {
		if b, err := scan(rows); err == nil {
			todo = append(todo, b)
		}
	}
	rows.Close()
	for _, b := range todo {
		if _, err := os.Stat(b.LocalPath); err != nil {
			continue
		}
		s.mu.Lock()
		s.upload(ctx, b.ID, b.App, b.LocalPath, filepath.Base(b.LocalPath), nil)
		s.mu.Unlock()
	}
}

func (s *Service) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return DefaultInterval
}

func (s *Service) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return DefaultRetention
}

// Policy returns the schedule interval and retention.
func (s *Service) Policy() (time.Duration, time.Duration) { return s.interval(), s.retention() }

// HostID identifies this server in object keys.
func (s *Service) HostID() string { return s.hostID }

// ---------------------------------------------------------------- S3 config

// S3Config returns the stored bucket config (ok=false if none).
func (s *Service) S3Config(ctx context.Context) (s3.Config, bool, error) {
	v, ok, err := s.Store.GetSetting(ctx, SettingS3)
	if err != nil || !ok {
		return s3.Config{}, false, err
	}
	b, err := s.Box.Open(v, purposeS3)
	if err != nil {
		return s3.Config{}, false, fmt.Errorf("stored S3 settings: %w", err)
	}
	var c s3.Config
	if err := json.Unmarshal(b, &c); err != nil {
		return s3.Config{}, false, err
	}
	return c, true, nil
}

// SetS3Config validates c with a test upload/download/delete and stores
// it sealed. An empty SecretKey keeps the stored secret.
func (s *Service) SetS3Config(ctx context.Context, c s3.Config) error {
	if c.SecretKey == "" {
		if old, ok, _ := s.S3Config(ctx); ok {
			c.SecretKey = old.SecretKey
		}
	}
	if err := c.Normalize(); err != nil {
		return err
	}
	cl, err := s3.New(c)
	if err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := cl.Test(tctx, c.Prefix); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	sealed, err := s.Box.Seal(b, purposeS3)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, SettingS3, sealed)
}

// RemoveS3Config deletes the bucket settings (backups stay local only).
func (s *Service) RemoveS3Config(ctx context.Context) error {
	_, err := s.Store.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingS3)
	return err
}

func (s *Service) client(ctx context.Context) (*s3.Client, s3.Config, bool, error) {
	c, ok, err := s.S3Config(ctx)
	if err != nil || !ok {
		return nil, c, false, err
	}
	cl, err := s3.New(c)
	return cl, c, err == nil, err
}

func (s *Service) keyPrefix(c s3.Config, app string) string {
	return c.Prefix + "/" + s.hostID + "/" + app + "/"
}

// ---------------------------------------------------------------- rows

const cols = `id, app, kind, status, object_key, local_path, size, sha256, files, release_id, error, created_at, finished_at`

func scan(r interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var c, f int64
	err := r.Scan(&b.ID, &b.App, &b.Kind, &b.Status, &b.ObjectKey, &b.LocalPath, &b.Size, &b.SHA256, &b.Files, &b.ReleaseID, &b.Error, &c, &f)
	b.CreatedAt = time.Unix(c, 0)
	if f != 0 {
		b.FinishedAt = time.Unix(f, 0)
	}
	return b, err
}

// List returns an app's backups, newest first.
func (s *Service) List(ctx context.Context, app string) ([]Backup, error) {
	rows, err := s.Store.Reader().QueryContext(ctx, `SELECT `+cols+` FROM backups WHERE app = ? ORDER BY id DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Get returns one backup.
func (s *Service) Get(ctx context.Context, id int64) (Backup, error) {
	b, err := scan(s.Store.Reader().QueryRowContext(ctx, `SELECT `+cols+` FROM backups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("backup #%d not found", id)
	}
	return b, err
}

// Latest returns the newest finished backup of app.
func (s *Service) Latest(ctx context.Context, app string) (Backup, bool) {
	b, err := scan(s.Store.Reader().QueryRowContext(ctx, `SELECT `+cols+` FROM backups WHERE app = ? AND status != ? ORDER BY id DESC LIMIT 1`, app, StatusRunning))
	return b, err == nil
}

// Problem describes why an app's latest backup needs attention ("" = fine).
// Backups whose upload is still in progress are skipped, so a problem
// stays visible until a backup actually succeeds.
func (s *Service) Problem(ctx context.Context, app string) string {
	rows, err := s.Store.Reader().QueryContext(ctx, `SELECT `+cols+` FROM backups WHERE app = ? AND status != ? ORDER BY id DESC LIMIT 10`, app, StatusRunning)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			return ""
		}
		switch {
		case b.Status == StatusFailed:
			return "last backup failed: " + b.Error
		case b.Error == uploadPending && time.Since(b.FinishedAt) < 30*time.Minute:
			continue
		case b.Error != "":
			return "last backup is only on this server: " + b.Error
		}
		return ""
	}
	return ""
}

func (s *Service) insert(ctx context.Context, app, kind, release string) (int64, error) {
	res, err := s.Store.Writer().ExecContext(ctx, `INSERT INTO backups (app, kind, status, release_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		app, kind, StatusRunning, release, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Service) exec(q string, args ...any) {
	if _, err := s.Store.Writer().Exec(q, args...); err != nil {
		s.Log.Error("backups table", "err", err)
	}
}

// ---------------------------------------------------------------- run

// Run takes a backup of app now. The archive is kept locally; the upload
// runs in the background when wait is false (pre-deploy), so the deploy
// downtime only includes the snapshot. It returns ErrNoDatabases (and
// records nothing) when DATA_DIR has no SQLite files.
func (s *Service) Run(ctx context.Context, app, kind string, wait bool, lg *logs.Log) (Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run(ctx, app, kind, wait, lg)
}

func note(lg *logs.Log, format string, a ...any) {
	if lg != nil {
		lg.Writef(format, a...)
	}
}

func (s *Service) run(ctx context.Context, app, kind string, wait bool, lg *logs.Log) (Backup, error) {
	dataDir := s.Layout.DataDir(app)
	dbs, err := FindDatabases(dataDir)
	if err != nil {
		return Backup{}, err
	}
	if len(dbs) == 0 {
		return Backup{}, ErrNoDatabases
	}
	release := ""
	if s.Hooks.Release != nil {
		release = s.Hooks.Release(app)
	}
	id, err := s.insert(ctx, app, kind, release)
	if err != nil {
		return Backup{}, err
	}
	start := time.Now()
	fail := func(err error) (Backup, error) {
		s.exec(`UPDATE backups SET status = ?, error = ?, finished_at = ? WHERE id = ?`, StatusFailed, err.Error(), time.Now().Unix(), id)
		s.Log.Error("backup failed", "app", app, "kind", kind, "err", err)
		b, _ := s.Get(context.WithoutCancel(ctx), id)
		return b, err
	}

	work, err := os.MkdirTemp(s.stagingDir(), app+"-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(work)
	m := &Manifest{Version: ManifestVersion, App: app, Kind: kind, CreatedAt: start.UTC(), Release: release, DootdVersion: buildinfo.Version}
	for _, rel := range dbs {
		dst := filepath.Join(work, "files", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fail(err)
		}
		if err := Snapshot(ctx, filepath.Join(dataDir, rel), dst); err != nil {
			return fail(fmt.Errorf("%s: %w", rel, err))
		}
		m.Files = append(m.Files, ManifestFile{Path: rel})
	}
	// The id keeps names unique even for two backups in the same second.
	name := fmt.Sprintf("%s-%s-%d.tar.zst", start.UTC().Format("20060102T150405Z"), kind, id)
	if err := os.MkdirAll(s.localDir(app), 0o700); err != nil {
		return fail(err)
	}
	local := filepath.Join(s.localDir(app), name)
	sum, size, err := writeArchive(local, m, filepath.Join(work, "files"))
	if err != nil {
		return fail(err)
	}
	s.exec(`UPDATE backups SET status = ?, local_path = ?, size = ?, sha256 = ?, files = ?, finished_at = ? WHERE id = ?`,
		StatusOK, local, size, sum, len(dbs), time.Now().Unix(), id)
	note(lg, "backup #%d: %d database(s), %s, snapshot took %s", id, len(dbs), human(size), time.Since(start).Round(10*time.Millisecond))
	s.Log.Info("backup taken", "app", app, "kind", kind, "id", id, "databases", len(dbs), "bytes", size)
	s.pruneLocal(app)

	if wait {
		s.upload(context.WithoutCancel(ctx), id, app, local, name, lg)
	} else {
		s.uploads.Add(1)
		go func() {
			defer s.uploads.Done()
			uctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			s.upload(uctx, id, app, local, name, nil)
		}()
	}
	b, err := s.Get(context.WithoutCancel(ctx), id)
	return b, err
}

// upload sends the archive to the bucket with retries, then applies
// retention. Without S3 settings the backup stays local only.
func (s *Service) upload(ctx context.Context, id int64, app, local, name string, lg *logs.Log) {
	cl, c, ok, err := s.client(ctx)
	if err != nil {
		s.exec(`UPDATE backups SET error = ? WHERE id = ?`, err.Error(), id)
		return
	}
	if !ok {
		note(lg, "backup #%d kept on this server only (no S3 bucket configured)", id)
		return
	}
	s.exec(`UPDATE backups SET error = ? WHERE id = ?`, uploadPending, id)
	key := s.keyPrefix(c, app) + name
	for attempt := 1; ; attempt++ {
		err = cl.PutFile(ctx, key, local, "application/zstd")
		if err == nil || attempt == uploadAttempts || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(attempt) * uploadRetryDelay)
	}
	if err != nil {
		s.exec(`UPDATE backups SET error = ? WHERE id = ?`, err.Error(), id)
		s.Log.Error("backup upload failed", "app", app, "id", id, "err", err)
		note(lg, "backup #%d upload failed: %v", id, err)
		return
	}
	s.exec(`UPDATE backups SET object_key = ?, error = '' WHERE id = ?`, key, id)
	note(lg, "backup #%d uploaded to %s", id, key)
	s.Log.Info("backup uploaded", "app", app, "id", id, "key", key)
	keep := s.retention()
	if app == SelfApp {
		keep = SelfRetention
	}
	if err := s.retain(ctx, cl, c, app, keep); err != nil {
		s.Log.Warn("backup retention", "app", app, "err", err)
	}
}

// retain deletes bucket objects of app older than keep, always keeping
// the newest one, and forgets rows that no longer have any copy.
func (s *Service) retain(ctx context.Context, cl *s3.Client, c s3.Config, app string, keep time.Duration) error {
	objs, err := cl.List(ctx, s.keyPrefix(c, app))
	if err != nil {
		return err
	}
	sort.Slice(objs, func(i, j int) bool { return objTime(objs[i]).After(objTime(objs[j])) })
	cutoff := time.Now().Add(-keep)
	var errs []error
	for i, o := range objs {
		if i == 0 || !objTime(o).Before(cutoff) {
			continue
		}
		if err := cl.Delete(ctx, o.Key); err != nil {
			errs = append(errs, err)
			continue
		}
		s.exec(`UPDATE backups SET object_key = '' WHERE object_key = ?`, o.Key)
	}
	s.forgetEmpty(app, cutoff)
	return errors.Join(errs...)
}

// objTime is the backup time from the key name (falls back to LastModified).
func objTime(o s3.Object) time.Time {
	base := filepath.Base(o.Key)
	if len(base) >= 16 {
		if t, err := time.Parse("20060102T150405Z", base[:16]); err == nil {
			return t
		}
	}
	return o.LastModified
}

// forgetEmpty drops successful rows that have no copy left, and failed
// rows older than cutoff (they keep the failure badge visible until then).
func (s *Service) forgetEmpty(app string, cutoff time.Time) {
	s.exec(`DELETE FROM backups WHERE app = ? AND (
		(status = ? AND object_key = '' AND local_path = '') OR (status = ? AND created_at < ?))`,
		app, StatusOK, StatusFailed, cutoff.Unix())
}

// pruneLocal keeps the newest KeepLocal archives of app on disk.
func (s *Service) pruneLocal(app string) {
	entries, _ := os.ReadDir(s.localDir(app))
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.zst") || strings.HasSuffix(e.Name(), ".db.zst") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names[min(len(names), KeepLocal):] {
		p := filepath.Join(s.localDir(app), n)
		os.Remove(p)
		s.exec(`UPDATE backups SET local_path = '' WHERE local_path = ?`, p)
	}
	if app != SelfApp {
		s.forgetEmpty(app, time.Now().Add(-s.retention()))
	}
}

// Wait blocks until background uploads finish (shutdown, tests).
func (s *Service) Wait() { s.uploads.Wait() }

// DeleteAll removes every backup of app: bucket objects, local copies and rows.
func (s *Service) DeleteAll(ctx context.Context, app string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if cl, c, ok, err := s.client(ctx); err != nil {
		errs = append(errs, err)
	} else if ok {
		objs, err := cl.List(ctx, s.keyPrefix(c, app))
		if err != nil {
			errs = append(errs, err)
		}
		for _, o := range objs {
			if err := cl.Delete(ctx, o.Key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := os.RemoveAll(s.localDir(app)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		s.exec(`DELETE FROM backups WHERE app = ?`, app)
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------- scheduler

// nextSlot is the next multiple of every (UTC) after t.
func nextSlot(t time.Time, every time.Duration) time.Time { return t.UTC().Truncate(every).Add(every) }

// Schedule runs the 3-hourly app backups and the daily dootd.db backup
// until ctx ends.
func (s *Service) Schedule(ctx context.Context) {
	go func() {
		s.UploadPending(ctx)
		s.maybeSelf(ctx)
		for {
			next := nextSlot(time.Now(), s.interval())
			s.Log.Info("next scheduled backup", "at", next.Format(time.RFC3339))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			s.RunAll(ctx, KindScheduled)
			s.maybeSelf(ctx)
			s.UploadPending(ctx)
		}
	}()
}

// RunAll backs up every app (one at a time).
func (s *Service) RunAll(ctx context.Context, kind string) {
	for _, app := range s.Hooks.Apps() {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.Run(ctx, app, kind, true, nil); err != nil && !errors.Is(err, ErrNoDatabases) {
			s.Log.Warn("scheduled backup failed", "app", app, "err", err)
		}
	}
}

func (s *Service) maybeSelf(ctx context.Context) {
	v, ok, _ := s.Store.GetSetting(ctx, settingSelfLast)
	if ok {
		if t, err := time.Parse(time.RFC3339, string(v)); err == nil && time.Since(t) < SelfInterval-10*time.Minute {
			return
		}
	}
	if _, err := s.BackupSelf(ctx); err != nil {
		s.Log.Warn("dootd.db backup failed", "err", err)
	}
}

// BackupSelf backs up dootd.db (zstd-compressed). Secrets inside it stay
// sealed, so the master key (recovery kit) is needed to use it.
func (s *Service) BackupSelf(ctx context.Context) (Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.insert(ctx, SelfApp, KindScheduled, "")
	if err != nil {
		return Backup{}, err
	}
	fail := func(err error) (Backup, error) {
		s.exec(`UPDATE backups SET status = ?, error = ?, finished_at = ? WHERE id = ?`, StatusFailed, err.Error(), time.Now().Unix(), id)
		return Backup{}, err
	}
	work, err := os.MkdirTemp(s.stagingDir(), "self-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(work)
	snap := filepath.Join(work, "dootd.db")
	if err := s.SelfSnapshot(ctx, snap); err != nil {
		return fail(err)
	}
	if err := QuickCheck(ctx, snap); err != nil {
		return fail(err)
	}
	now := time.Now()
	name := fmt.Sprintf("%s-%d-dootd.db.zst", now.UTC().Format("20060102T150405Z"), id)
	os.MkdirAll(s.localDir(SelfApp), 0o700)
	local := filepath.Join(s.localDir(SelfApp), name)
	sum, size, err := compressFile(snap, local)
	if err != nil {
		return fail(err)
	}
	s.exec(`UPDATE backups SET status = ?, local_path = ?, size = ?, sha256 = ?, files = 1, finished_at = ? WHERE id = ?`,
		StatusOK, local, size, sum, time.Now().Unix(), id)
	s.Store.SetSetting(ctx, settingSelfLast, []byte(now.UTC().Format(time.RFC3339)))
	s.pruneLocal(SelfApp)
	s.upload(ctx, id, SelfApp, local, name, nil)
	s.Log.Info("dootd.db backed up", "id", id, "bytes", size)
	return s.Get(ctx, id)
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
