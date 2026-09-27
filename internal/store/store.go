// Package store owns dootd's state database (SQLite, WAL mode).
//
// All writes go through a single-connection writer pool so SQLite never sees
// competing writers from dootd itself. Reads use a separate read-only pool.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGO)
)

// Store is the handle to dootd.db.
type Store struct {
	writer *sql.DB
	reader *sql.DB
}

// Open opens (creating if needed) the database at path and applies all
// pending migrations. It returns an error if the schema is newer than this
// binary understands or if any migration fails.
func Open(ctx context.Context, path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve path: %w", err)
	}

	writer, err := sql.Open("sqlite", dsn(abs, false))
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)

	if err := writer.PingContext(ctx); err != nil {
		writer.Close()
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}

	if err := migrate(ctx, writer, embeddedMigrations); err != nil {
		writer.Close()
		return nil, err
	}

	reader, err := sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	reader.SetMaxOpenConns(max(4, runtime.NumCPU()))
	reader.SetMaxIdleConns(2)
	reader.SetConnMaxIdleTime(5 * time.Minute)

	if err := reader.PingContext(ctx); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}

	return &Store{writer: writer, reader: reader}, nil
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		// BEGIN IMMEDIATE takes the write lock up front, avoiding
		// SQLITE_BUSY upgrades mid-transaction.
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// Writer returns the single-connection pool used for all writes.
func (s *Store) Writer() *sql.DB { return s.writer }

// Reader returns the read-only pool.
func (s *Store) Reader() *sql.DB { return s.reader }

// Close closes both pools.
func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close())
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	return currentVersion(ctx, s.reader)
}

// GetSetting returns the raw value for key. Encrypted settings are stored
// sealed by the secrets package; callers are responsible for opening them.
func (s *Store) GetSetting(ctx context.Context, key string) ([]byte, bool, error) {
	var v []byte
	err := s.reader.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get setting %q: %w", key, err)
	}
	return v, true, nil
}

// SetSetting inserts or replaces the value for key.
func (s *Store) SetSetting(ctx context.Context, key string, value []byte) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Unix())
	if err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}
