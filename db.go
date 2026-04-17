package hubsync

import (
	"fmt"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// OpenDB opens a SQLite database at the given path with WAL mode.
func OpenDB(path string) (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// OpenDBReadOnly opens the hub DB in read-only mode. Safe to call while a
// different process holds the write lock (SQLite WAL permits concurrent
// readers). Used by the CLI's ls/status fallback when no serve is running.
func OpenDBReadOnly(path string) (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open db %s (ro): %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
