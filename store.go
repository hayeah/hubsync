package hubsync

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// changeLogRow is the DB row representation for change_log.
type changeLogRow struct {
	Version int64  `db:"version"`
	Path    string `db:"path"`
	Op      string `db:"op"`
	Kind    int    `db:"kind"`
	Digest  []byte `db:"digest"`
	Size    int64  `db:"size"`
	Mode    uint32 `db:"mode"`
	MTime   int64  `db:"mtime"`
}

func (r changeLogRow) toChangeEntry() ChangeEntry {
	return ChangeEntry{
		Version: r.Version,
		Path:    r.Path,
		Op:      ChangeOp(r.Op),
		Kind:    FileKind(r.Kind),
		Digest:  Digest(r.Digest),
		Size:    r.Size,
		Mode:    r.Mode,
		MTime:   r.MTime,
	}
}

// ArchiveState is the archive column of a hub_entry row.
// Empty string represents NULL (archive worker hasn't processed yet).
type ArchiveState string

const (
	ArchiveStateDirty    ArchiveState = "dirty"
	ArchiveStateArchived ArchiveState = "archived"
	ArchiveStateUnpinned ArchiveState = "unpinned"
)

// HubEntry is a materialized tree row plus archive state. Single owner at a
// time per column set (scanner owns path/digest/..., archive worker owns the
// archive_* columns).
type HubEntry struct {
	Path              string
	Kind              FileKind
	Digest            Digest
	Size              int64
	Mode              uint32
	MTime             int64
	Version           int64
	ArchiveState      ArchiveState
	ArchiveFileID     string
	ArchiveSHA1       []byte
	ArchiveUploadedAt int64
	UpdatedAt         int64
}

type hubEntryRow struct {
	Path              string         `db:"path"`
	Kind              int            `db:"kind"`
	Digest            []byte         `db:"digest"`
	Size              sql.NullInt64  `db:"size"`
	Mode              sql.NullInt64  `db:"mode"`
	MTime             sql.NullInt64  `db:"mtime"`
	Version           int64          `db:"version"`
	ArchiveState      sql.NullString `db:"archive_state"`
	ArchiveFileID     sql.NullString `db:"archive_file_id"`
	ArchiveSHA1       []byte         `db:"archive_sha1"`
	ArchiveUploadedAt sql.NullInt64  `db:"archive_uploaded_at"`
	UpdatedAt         int64          `db:"updated_at"`
}

func (r hubEntryRow) toEntry() HubEntry {
	e := HubEntry{
		Path:      r.Path,
		Kind:      FileKind(r.Kind),
		Digest:    Digest(r.Digest),
		Version:   r.Version,
		UpdatedAt: r.UpdatedAt,
	}
	if r.Size.Valid {
		e.Size = r.Size.Int64
	}
	if r.Mode.Valid {
		e.Mode = uint32(r.Mode.Int64)
	}
	if r.MTime.Valid {
		e.MTime = r.MTime.Int64
	}
	if r.ArchiveState.Valid {
		e.ArchiveState = ArchiveState(r.ArchiveState.String)
	}
	if r.ArchiveFileID.Valid {
		e.ArchiveFileID = r.ArchiveFileID.String
	}
	e.ArchiveSHA1 = r.ArchiveSHA1
	if r.ArchiveUploadedAt.Valid {
		e.ArchiveUploadedAt = r.ArchiveUploadedAt.Int64
	}
	return e
}

// TreeEntry projection (drops archive columns) for backwards-compat with
// callers that don't need them.
func (e HubEntry) TreeEntry() TreeEntry {
	return TreeEntry{
		Path:   e.Path,
		Kind:   e.Kind,
		Digest: e.Digest,
		Size:   e.Size,
		Mode:   e.Mode,
		MTime:  e.MTime,
	}
}

// HubStore manages the hub's SQLite database: change_log (append-only event
// log) and hub_entry (materialized tree + archive state).
type HubStore struct {
	DB *sqlx.DB
	mu sync.Mutex
}

// NewHubStore opens the hub DB, initializes the schema, and persists/validates
// the configured hash algo in hub_config_cache. Returns a cleanup func that
// closes the DB.
func NewHubStore(cfg HubStoreConfig) (*HubStore, func(), error) {
	db, err := OpenDB(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	if err := initHubSchema(db); err != nil {
		db.Close()
		return nil, nil, err
	}
	s := &HubStore{DB: db}
	if cfg.Hasher != nil {
		if err := s.ensureHashAlgo(cfg.Hasher.Name()); err != nil {
			db.Close()
			return nil, nil, err
		}
	}
	return s, func() { db.Close() }, nil
}

// ensureHashAlgo persists the configured hash algo on first use and errors
// out if a subsequent run requests a different algorithm against the same DB.
func (s *HubStore) ensureHashAlgo(algo string) error {
	existing, ok, err := s.ConfigCacheGet("hash_algo")
	if err != nil {
		return err
	}
	if !ok {
		return s.ConfigCacheSet("hash_algo", algo)
	}
	if existing != algo {
		return fmt.Errorf(
			"hub DB was initialized with hash=%q but config requests hash=%q; wipe .hubsync/ and re-bootstrap to switch algorithms",
			existing, algo,
		)
	}
	return nil
}

func initHubSchema(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS change_log (
			version   INTEGER PRIMARY KEY AUTOINCREMENT,
			path      TEXT NOT NULL,
			op        TEXT NOT NULL CHECK(op IN ('create', 'update', 'delete')),
			kind      INTEGER NOT NULL DEFAULT 0,
			digest    BLOB,
			size      INTEGER,
			mode      INTEGER,
			mtime     INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_change_log_path ON change_log(path);

		CREATE TABLE IF NOT EXISTS hub_entry (
			path                 TEXT PRIMARY KEY,
			kind                 INTEGER NOT NULL,
			digest               BLOB,
			size                 INTEGER,
			mode                 INTEGER,
			mtime                INTEGER,
			version              INTEGER NOT NULL,
			archive_state        TEXT,
			archive_file_id      TEXT,
			archive_sha1         BLOB,
			archive_uploaded_at  INTEGER,
			updated_at           INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_hub_entry_digest ON hub_entry(digest);
		CREATE INDEX IF NOT EXISTS idx_hub_entry_archive_state ON hub_entry(archive_state);

		CREATE TABLE IF NOT EXISTS hub_config_cache (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	return err
}

// Append inserts a change_log row and upserts the matching hub_entry row in
// a single transaction. Returns the assigned version number.
func (s *HubStore) Append(entry ChangeEntry) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.DB.Beginx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var digestBlob []byte
	if !entry.Digest.IsZero() {
		digestBlob = entry.Digest.Bytes()
	}

	res, err := tx.Exec(
		`INSERT INTO change_log (path, op, kind, digest, size, mode, mtime) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.Path, string(entry.Op), int(entry.Kind), digestBlob, entry.Size, entry.Mode, entry.MTime,
	)
	if err != nil {
		return 0, fmt.Errorf("insert change_log: %w", err)
	}
	version, _ := res.LastInsertId()

	now := time.Now().Unix()
	if entry.Op == OpDelete {
		if _, err := tx.Exec(`DELETE FROM hub_entry WHERE path = ?`, entry.Path); err != nil {
			return 0, fmt.Errorf("delete hub_entry: %w", err)
		}
	} else {
		// Upsert: preserve archive_* columns if row exists, reset them to NULL
		// on digest change (archive needs to re-upload). A simpler impl flips
		// state to 'dirty' on content change; we do that in a follow-up after
		// schema land.
		if _, err := tx.Exec(
			`INSERT INTO hub_entry (path, kind, digest, size, mode, mtime, version, updated_at)
			   VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(path) DO UPDATE SET
			   kind       = excluded.kind,
			   digest     = excluded.digest,
			   size       = excluded.size,
			   mode       = excluded.mode,
			   mtime      = excluded.mtime,
			   version    = excluded.version,
			   updated_at = excluded.updated_at,
			   archive_state = CASE
			     WHEN hub_entry.digest IS NOT excluded.digest THEN 'dirty'
			     ELSE hub_entry.archive_state
			   END`,
			entry.Path, int(entry.Kind), digestBlob, entry.Size, entry.Mode, entry.MTime, version, now,
		); err != nil {
			return 0, fmt.Errorf("upsert hub_entry: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return version, nil
}

// TreeSnapshot returns the materialized tree (scanner columns only) as a map
// keyed by path. Ignores archive_state.
func (s *HubStore) TreeSnapshot() map[string]TreeEntry {
	var rows []hubEntryRow
	if err := s.DB.Select(&rows, `SELECT * FROM hub_entry`); err != nil {
		return map[string]TreeEntry{}
	}
	m := make(map[string]TreeEntry, len(rows))
	for _, r := range rows {
		e := r.toEntry()
		m[e.Path] = e.TreeEntry()
	}
	return m
}

// EntrySnapshot returns every hub_entry row (includes archive state).
func (s *HubStore) EntrySnapshot() ([]HubEntry, error) {
	var rows []hubEntryRow
	if err := s.DB.Select(&rows, `SELECT * FROM hub_entry`); err != nil {
		return nil, err
	}
	out := make([]HubEntry, len(rows))
	for i, r := range rows {
		out[i] = r.toEntry()
	}
	return out, nil
}

// TreeLookup returns a single TreeEntry for path.
func (s *HubStore) TreeLookup(path string) (TreeEntry, bool) {
	e, ok, err := s.EntryLookup(path)
	if err != nil || !ok {
		return TreeEntry{}, false
	}
	return e.TreeEntry(), true
}

// EntryLookup returns a single HubEntry (with archive state) for path.
func (s *HubStore) EntryLookup(path string) (HubEntry, bool, error) {
	var r hubEntryRow
	err := s.DB.Get(&r, `SELECT * FROM hub_entry WHERE path = ?`, path)
	if err == sql.ErrNoRows {
		return HubEntry{}, false, nil
	}
	if err != nil {
		return HubEntry{}, false, err
	}
	return r.toEntry(), true, nil
}

// PathByDigest finds a path whose file-row digest matches. Ignores
// directories and unpinned-only rows (which have no local bytes to serve).
func (s *HubStore) PathByDigest(digest Digest) (string, bool) {
	if digest.IsZero() {
		return "", false
	}
	var path string
	err := s.DB.Get(&path,
		`SELECT path FROM hub_entry
		  WHERE digest = ? AND kind = 0
		  LIMIT 1`,
		digest.Bytes(),
	)
	if err != nil {
		return "", false
	}
	return path, true
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

// ConfigCacheGet reads a value from hub_config_cache (for detecting destructive
// config changes like the hash algo switching).
func (s *HubStore) ConfigCacheGet(key string) (string, bool, error) {
	var v string
	err := s.DB.Get(&v, `SELECT value FROM hub_config_cache WHERE key = ?`, key)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ConfigCacheSet writes a value into hub_config_cache.
func (s *HubStore) ConfigCacheSet(key, value string) error {
	_, err := s.DB.Exec(
		`INSERT INTO hub_config_cache (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}
