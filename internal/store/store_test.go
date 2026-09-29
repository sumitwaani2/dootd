package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestOpenFreshAndReopen(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "dootd.db")
	ms, err := loadMigrations(embeddedMigrations)
	if err != nil {
		t.Fatal(err)
	}
	latest := len(ms)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.SchemaVersion(ctx)
	if v != latest {
		t.Fatalf("schema %d, want %d", v, latest)
	}
	var mode string
	s.Reader().QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode %q", mode)
	}
	if err := s.SetSetting(ctx, "k", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	s.SetSetting(ctx, "k", []byte("v2"))
	if _, err := s.Reader().Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('x', 'y', 0)`); err == nil {
		t.Fatal("reader pool must be read-only")
	}
	s.Close()

	s, err = Open(ctx, p) // idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, ok, err := s.GetSetting(ctx, "k")
	if err != nil || !ok || string(b) != "v2" {
		t.Fatalf("setting: %q %v %v", b, ok, err)
	}
	if _, ok, _ := s.GetSetting(ctx, "missing"); ok {
		t.Fatal("missing setting found")
	}
}

func openRaw(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "t.db"), false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFailingMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0002_b.sql": {Data: []byte(`CREATE TABLE b (x INTEGER); INSERT INTO nope VALUES (1);`)},
	}
	err := migrate(ctx, db, fsys)
	if err == nil || !strings.Contains(err.Error(), "0002_b failed") {
		t.Fatalf("got %v", err)
	}
	v, _ := currentVersion(ctx, db)
	if v != 1 {
		t.Fatalf("version %d, want 1 (earlier migration kept)", v)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'b'`).Scan(&n)
	if n != 0 {
		t.Fatal("table from the failed migration exists")
	}
}

func TestSchemaTooNew(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	two := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0002_b.sql": {Data: []byte(`CREATE TABLE b (x INTEGER);`)},
	}
	if err := migrate(ctx, db, two); err != nil {
		t.Fatal(err)
	}
	one := fstest.MapFS{"0001_a.sql": two["0001_a.sql"]}
	err := migrate(ctx, db, one)
	if !errors.Is(err, ErrSchemaTooNew) || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("got %v", err)
	}
}

func TestMigrationNames(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"gap":      {"0001_a.sql": {}, "0003_c.sql": {}},
		"bad name": {"0001_A.sql": {}},
		"not one":  {"0002_b.sql": {}},
	} {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if ms, err := loadMigrations(embeddedMigrations); err != nil || len(ms) == 0 {
		t.Fatalf("embedded migrations: %v", err)
	}
}
