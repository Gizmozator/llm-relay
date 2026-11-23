// Package db opens the SQLite database and applies embedded migrations.
package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens the database at path with WAL mode and sane busy handling.
// The parent directory is created if missing (fresh installs, scratch images).
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("db dir %s: %w", dir, err)
		}
	}
	// _txlock=immediate: every Begin() starts BEGIN IMMEDIATE, taking the
	// write lock up front so busy_timeout(5000) waits out contention. With
	// the default deferred mode a tx that reads before writing (all guard
	// checks here) takes a read snapshot first; a commit on another
	// connection — the relay writes usage rows on every request — then
	// fails the write with SQLITE_BUSY_SNAPSHOT, which the busy handler
	// does NOT wait out. All Begin() users are write paths; plain
	// Query/QueryRow calls outside txs are unaffected and keep WAL
	// read concurrency.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}
	// One writer at a time keeps SQLite happy; a few readers may run in
	// parallel against the WAL snapshot.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}
	// The DB holds password hashes, API keys and backend credentials: keep it
	// owner-only even if the file or an earlier umask left it world-readable.
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		db.Close()
		return nil, fmt.Errorf("db chmod %s: %w", path, err)
	}
	return db, nil
}

// Migrate applies embedded .sql files in filename order, each in a
// transaction, tracked in schema_version. Re-running is a no-op.
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("schema_version: %w", err)
	}

	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = ?`, name).Scan(&exists); err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		if exists > 0 {
			continue
		}
		content, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		log.Printf("migration applied: %s", name)
	}
	return nil
}
