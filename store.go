package hubsync

import (
	"database/sql"
	"fmt"
	"sync"

	"github.com/jmoiron/sqlx"
)

// changeLogRow is the DB row representation for change_log.
type changeLogRow struct {
	Version int64          `db:"version"`
	Path    string         `db:"path"`
	Op      string         `db:"op"`
	Kind    int            `db:"kind"`
	Digest  sql.NullString `db:"digest"`
	Size    int64          `db:"size"`
	Mode    uint32         `db:"mode"`
	MTime   int64          `db:"mtime"`
}

func (r changeLogRow) toChangeEntry() ChangeEntry {
	e := ChangeEntry{
		Version: r.Version,
		Path:    r.Path,
		Op:      ChangeOp(r.Op),
		Kind:    FileKind(r.Kind),
		Size:    r.Size,
		Mode:    r.Mode,
		MTime:   r.MTime,
	}
	if r.Digest.Valid {
		e.Digest, _ = ParseDigest(r.Digest.String)
	}
	return e
}

// HubStore manages the hub's SQLite database: change_log and materialized tree.
type HubStore struct {
	DB   *sqlx.DB
	mu   sync.Mutex
	tree map[string]TreeEntry
}

// NewHubStore creates a HubStore and initializes the schema.
func NewHubStore(db *sqlx.DB) (*HubStore, error) {
	if err := initHubSchema(db); err != nil {
		return nil, err
	}
	s := &HubStore{DB: db, tree: make(map[string]TreeEntry)}
	if err := s.loadTree(); err != nil {
		return nil, fmt.Errorf("load tree: %w", err)
	}
	return s, nil
}

func initHubSchema(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS change_log (
			version   INTEGER PRIMARY KEY AUTOINCREMENT,
			path      TEXT NOT NULL,
			op        TEXT NOT NULL CHECK(op IN ('create', 'update', 'delete')),
			kind      INTEGER NOT NULL DEFAULT 0,
			digest    TEXT,
			size      INTEGER,
			mode      INTEGER,
			mtime     INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_change_log_path ON change_log(path);
	`)
	return err
}

func (s *HubStore) loadTree() error {
	var rows []changeLogRow
	if err := s.DB.Select(&rows, `SELECT version, path, op, kind, digest, size, mode, mtime FROM change_log ORDER BY version`); err != nil {
		return err
	}
	for _, r := range rows {
		if r.Op == string(OpDelete) {
			delete(s.tree, r.Path)
		} else {
			e := r.toChangeEntry()
			s.tree[r.Path] = TreeEntry{
				Path:   e.Path,
				Kind:   e.Kind,
				Digest: e.Digest,
				Size:   e.Size,
				Mode:   e.Mode,
				MTime:  e.MTime,
			}
		}
	}
	return nil
}

// Append writes a change entry to the log and updates the in-memory tree.
// Returns the assigned version number.
func (s *HubStore) Append(entry ChangeEntry) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var digestHex *string
	if !entry.Digest.IsZero() {
		h := entry.Digest.Hex()
		digestHex = &h
	}

	res, err := s.DB.Exec(
		`INSERT INTO change_log (path, op, kind, digest, size, mode, mtime) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.Path, string(entry.Op), int(entry.Kind), digestHex, entry.Size, entry.Mode, entry.MTime,
	)
	if err != nil {
		return 0, fmt.Errorf("insert change_log: %w", err)
	}
	version, _ := res.LastInsertId()

	if entry.Op == OpDelete {
		delete(s.tree, entry.Path)
	} else {
		s.tree[entry.Path] = TreeEntry{
			Path:   entry.Path,
			Kind:   entry.Kind,
			Digest: entry.Digest,
			Size:   entry.Size,
			Mode:   entry.Mode,
			MTime:  entry.MTime,
		}
	}

	return version, nil
}

// TreeSnapshot returns a copy of the current materialized tree.
func (s *HubStore) TreeSnapshot() map[string]TreeEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]TreeEntry, len(s.tree))
	for k, v := range s.tree {
		cp[k] = v
	}
	return cp
}

// TreeLookup returns a single entry from the current tree.
func (s *HubStore) TreeLookup(path string) (TreeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tree[path]
	return e, ok
}

// PathByDigest finds a path in the current tree with the given digest.
func (s *HubStore) PathByDigest(digest Digest) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.tree {
		if e.Digest == digest && e.Kind == FileKindFile {
			return e.Path, true
		}
	}
	return "", false
}

// ChangesSince returns all change_log entries with version > since.
func (s *HubStore) ChangesSince(since int64) ([]ChangeEntry, error) {
	var rows []changeLogRow
	if err := s.DB.Select(&rows,
		`SELECT version, path, op, kind, digest, size, mode, mtime FROM change_log WHERE version > ? ORDER BY version`,
		since,
	); err != nil {
		return nil, err
	}
	entries := make([]ChangeEntry, len(rows))
	for i, r := range rows {
		entries[i] = r.toChangeEntry()
	}
	return entries, nil
}

// LatestVersion returns the highest version in the change log, or 0 if empty.
func (s *HubStore) LatestVersion() (int64, error) {
	var v sql.NullInt64
	if err := s.DB.Get(&v, `SELECT MAX(version) FROM change_log`); err != nil {
		return 0, err
	}
	return v.Int64, nil
}
