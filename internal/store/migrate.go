package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// embeddedMigrations is the migrations directory compiled into the binary.
var embeddedMigrations = mustSub(migrationFiles, "migrations")

// ErrSchemaTooNew means the database was migrated by a newer dootd.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this dootd binary supports")

// Migration file names: NNNN_short_name.sql, e.g. 0001_init.sql.
// Rules: never edit an applied migration; never put BEGIN/COMMIT in a file
// (the runner wraps each file in its own transaction).
var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version int
	name    string
	sql     string
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// loadMigrations reads and validates migrations from fsys: names must match
// the pattern and versions must be unique and contiguous starting at 1.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("store: invalid migration file name %q (want NNNN_name.sql)", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: m[2], sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migrations must be numbered 0001, 0002, ... without gaps or duplicates; found %04d at position %d", m.version, i+1)
		}
	}
	return ms, nil
}

func currentVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return int(v.Int64), nil
}

// migrate applies all pending migrations from fsys, each in its own
// transaction together with its schema_migrations row.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	cur, err := currentVersion(ctx, db)
	if err != nil {
		return err
	}
	if cur > len(ms) {
		return fmt.Errorf("%w (database at version %d, binary knows up to %d)", ErrSchemaTooNew, cur, len(ms))
	}

	for _, m := range ms[cur:] {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migration %04d_%s: begin: %w", m.version, m.name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: migration %04d_%s failed: %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().UTC().Unix()); err != nil {
		return fmt.Errorf("store: migration %04d_%s: record: %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %04d_%s: commit: %w", m.version, m.name, err)
	}
	return nil
}
