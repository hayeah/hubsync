package taskrunner

import (
	"database/sql"
	"fmt"
)

// ensureTasksTable creates the runner-owned tables if missing. The items
// table is materialized lazily on first plan (when we know the struct's
// JSON shape); everything else has a fixed schema.
func ensureTasksTable(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id            TEXT PRIMARY KEY,
			status        TEXT NOT NULL DEFAULT 'pending',
			attempts      INTEGER NOT NULL DEFAULT 0,
			started_at    TIMESTAMP,
			ended_at      TIMESTAMP,
			heartbeat_at  TIMESTAMP,
			error         TEXT,
			meta          JSON NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status)`,
		`CREATE TABLE IF NOT EXISTS runs (
			run_id     TEXT PRIMARY KEY,
			started_at TIMESTAMP,
			ended_at   TIMESTAMP,
			argv       JSON
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("taskrunner: ensure schema: %w", err)
		}
	}
	return nil
}

// itemsEmpty reports whether the items table exists and is empty (or
// missing entirely). Used to decide whether to invoke Plan.
func itemsEmpty(db *sql.DB) (bool, error) {
	var exists int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		  WHERE type='table' AND name='items'`,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("taskrunner: probe items table: %w", err)
	}
	if exists == 0 {
		return true, nil
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		return false, fmt.Errorf("taskrunner: count items: %w", err)
	}
	return n == 0, nil
}
