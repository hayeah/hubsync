package taskrunner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// archiveTimestampLayout is a filesystem-safe UTC stamp — ISO-8601 basic
// on the time portion (no colons), extended on the date portion. Matches
// the design-doc example "2026-04-19T143522Z". Always UTC.
const archiveTimestampLayout = "2006-01-02T150405Z"

// archiveIfDrained moves the DB file out of the canonical path after a
// successful drain so the next invocation plans fresh. Bails without
// error when archiving does not apply (in-memory DB, KeepDB set, or
// residual work in the queue). Idempotent: losing a rename race against
// another process that already moved the file is treated as success.
func (r *Runner[T]) archiveIfDrained(ctx context.Context, opts RunOptions) error {
	if opts.KeepDB {
		return nil
	}
	if r.cfg.DBPath == "" {
		// In-memory DB — nothing to rename. Archive-on-success has no
		// meaning without a file path.
		return nil
	}

	drained, err := queueDrained(ctx, r.db)
	if err != nil {
		return err
	}
	if !drained {
		return nil
	}

	// Merge WAL into the main .db so the archive is self-contained
	// without the -wal / -shm sidecars.
	if _, err := r.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("taskrunner: wal_checkpoint before archive: %w", err)
	}
	if err := r.Close(); err != nil {
		return fmt.Errorf("taskrunner: close before archive: %w", err)
	}

	src := r.cfg.DBPath
	dst := archiveDestPath(src, time.Now().UTC())

	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Another process won the rename race. The archive happened;
			// we just don't own the destination name. Swallow the error
			// and leave archivedPath empty so callers know *this* process
			// didn't do the move.
			return nil
		}
		return fmt.Errorf("taskrunner: archive rename %s -> %s: %w", src, dst, err)
	}

	// Best-effort cleanup of SQLite WAL sidecars. After the checkpoint+
	// close above they should be absent (or empty), but a crashed run
	// could leave stragglers that would otherwise confuse a "fresh"
	// plan against the canonical path.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(src + suffix)
	}

	r.archivedPath = dst
	fmt.Fprintf(os.Stderr, "taskrunner: archived task DB -> %s\n", dst)
	return nil
}

// queueDrained reports whether every row in tasks is terminal-done. Any
// pending/running/failed row (stale-running included — a caller that
// bails out mid-drain should NOT trigger archiving) means work remains.
func queueDrained(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks
		  WHERE status IN ('pending','running','failed')`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("taskrunner: probe drain: %w", err)
	}
	return n == 0, nil
}

// archiveDestPath returns the destination path for archiving src at t.
// The timestamp is prefixed with a trailing dash and prepended to the
// source's basename; the directory is preserved. Exported-for-testing
// via lowercased name since this package's tests live alongside.
func archiveDestPath(src string, t time.Time) string {
	dir, base := filepath.Split(src)
	stamp := t.UTC().Format(archiveTimestampLayout)
	return filepath.Join(dir, stamp+"-"+base)
}
