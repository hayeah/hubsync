package hubsync

import (
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ClientStore manages the client's SQLite database: hub_tree and sync_state.
type ClientStore struct {
	DB *sqlx.DB
}

// NewClientStore creates a ClientStore and initializes the schema.
func NewClientStore(db *sqlx.DB) (*ClientStore, error) {
	if err := initClientSchema(db); err != nil {
		return nil, err
	}
	return &ClientStore{DB: db}, nil
}

func initClientSchema(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS hub_tree (
			path    TEXT PRIMARY KEY,
			kind    INTEGER NOT NULL DEFAULT 0,
			digest  TEXT,
			size    INTEGER,
			mode    INTEGER,
			mtime   INTEGER,
			version INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_hub_tree_digest ON hub_tree(digest);

		CREATE TABLE IF NOT EXISTS sync_state (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	return err
}

// HubVersion returns the last synced hub version, or 0 if never synced.
func (s *ClientStore) HubVersion() (int64, error) {
	var v sql.NullString
	err := s.DB.Get(&v, `SELECT value FROM sync_state WHERE key = 'hub_version'`)
	if err == sql.ErrNoRows || !v.Valid {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var version int64
	fmt.Sscanf(v.String, "%d", &version)
	return version, nil
}

// SetHubVersion updates the last synced hub version.
func (s *ClientStore) SetHubVersion(version int64) error {
	_, err := s.DB.Exec(
		`INSERT OR REPLACE INTO sync_state (key, value) VALUES ('hub_version', ?)`,
		fmt.Sprintf("%d", version),
	)
	return err
}

// UpsertEntry inserts or replaces an entry in hub_tree.
func (s *ClientStore) UpsertEntry(path string, kind FileKind, digest Digest, size int64, mode uint32, mtime int64, version int64) error {
	var digestHex *string
	if !digest.IsZero() {
		h := digest.Hex()
		digestHex = &h
	}
	_, err := s.DB.Exec(
		`INSERT OR REPLACE INTO hub_tree (path, kind, digest, size, mode, mtime, version) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		path, int(kind), digestHex, size, mode, mtime, version,
	)
	return err
}

// DeleteEntry removes an entry from hub_tree.
func (s *ClientStore) DeleteEntry(path string) error {
	_, err := s.DB.Exec(`DELETE FROM hub_tree WHERE path = ?`, path)
	return err
}

// LookupByDigest finds a path with the given digest (for dedup/copy).
func (s *ClientStore) LookupByDigest(digest Digest, excludePath string) (string, bool, error) {
	h := digest.Hex()
	var path string
	err := s.DB.Get(&path,
		`SELECT path FROM hub_tree WHERE digest = ? AND path != ? LIMIT 1`,
		h, excludePath,
	)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}

// ApplyChange applies a single sync event to the client store within a transaction.
func (s *ClientStore) ApplyChange(version int64, path string, op ChangeOp, kind FileKind, digest Digest, size int64, mode uint32, mtime int64) error {
	tx, err := s.DB.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if op == OpDelete {
		if _, err := tx.Exec(`DELETE FROM hub_tree WHERE path = ?`, path); err != nil {
			return err
		}
	} else {
		var digestHex *string
		if !digest.IsZero() {
			h := digest.Hex()
			digestHex = &h
		}
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO hub_tree (path, kind, digest, size, mode, mtime, version) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			path, int(kind), digestHex, size, mode, mtime, version,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO sync_state (key, value) VALUES ('hub_version', ?)`,
		fmt.Sprintf("%d", version),
	); err != nil {
		return err
	}

	return tx.Commit()
}
