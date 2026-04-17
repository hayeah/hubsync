package hubsync

import (
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ClientStore manages the client's SQLite database: hub_tree + sync_state.
type ClientStore struct {
	DB *sqlx.DB
}

// NewClientStore creates a ClientStore and initializes the schema against an
// already-opened DB. For wire integration, use OpenClientStore.
func NewClientStore(db *sqlx.DB) (*ClientStore, error) {
	if err := initClientSchema(db); err != nil {
		return nil, err
	}
	return &ClientStore{DB: db}, nil
}

// OpenClientStore opens the client DB, initializes the schema, and returns a
// cleanup func. This is the wire-provided constructor.
func OpenClientStore(cfg ClientStoreConfig) (*ClientStore, func(), error) {
	db, err := OpenDB(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	s, err := NewClientStore(db)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return s, func() { db.Close() }, nil
}

func initClientSchema(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS hub_tree (
			path    TEXT PRIMARY KEY,
			kind    INTEGER NOT NULL DEFAULT 0,
			digest  BLOB,
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

// HashAlgo returns the hub's configured hash algorithm (propagated via
// snapshot). Empty if the snapshot didn't carry the value.
func (s *ClientStore) HashAlgo() (string, error) {
	var v sql.NullString
	err := s.DB.Get(&v, `SELECT value FROM sync_state WHERE key = 'hash_algo'`)
	if err == sql.ErrNoRows || !v.Valid {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v.String, nil
}

// SetHashAlgo stores the hub's configured hash algorithm.
func (s *ClientStore) SetHashAlgo(algo string) error {
	_, err := s.DB.Exec(
		`INSERT OR REPLACE INTO sync_state (key, value) VALUES ('hash_algo', ?)`,
		algo,
	)
	return err
}

// UpsertEntry inserts or replaces an entry in hub_tree.
func (s *ClientStore) UpsertEntry(path string, kind FileKind, digest Digest, size int64, mode uint32, mtime int64, version int64) error {
	var digestBlob []byte
	if !digest.IsZero() {
		digestBlob = digest.Bytes()
	}
	_, err := s.DB.Exec(
		`INSERT OR REPLACE INTO hub_tree (path, kind, digest, size, mode, mtime, version) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		path, int(kind), digestBlob, size, mode, mtime, version,
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
	if digest.IsZero() {
		return "", false, nil
	}
	var path string
	err := s.DB.Get(&path,
		`SELECT path FROM hub_tree WHERE digest = ? AND path != ? LIMIT 1`,
		digest.Bytes(), excludePath,
	)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}

// hubTreeRow represents a row from the hub_tree table.
type hubTreeRow struct {
	Path    string `db:"path"`
	Kind    int    `db:"kind"`
	Digest  []byte `db:"digest"`
	Size    int64  `db:"size"`
	Mode    uint32 `db:"mode"`
	MTime   int64  `db:"mtime"`
	Version int64  `db:"version"`
}

// HubTreeEntry represents an entry in the client's hub_tree with its version.
type HubTreeEntry struct {
	TreeEntry
	Version int64
}

// TreeSnapshot returns all entries from hub_tree as a map.
func (s *ClientStore) TreeSnapshot() (map[string]HubTreeEntry, error) {
	var rows []hubTreeRow
	if err := s.DB.Select(&rows, `SELECT path, kind, digest, size, mode, mtime, version FROM hub_tree`); err != nil {
		return nil, err
	}
	result := make(map[string]HubTreeEntry, len(rows))
	for _, r := range rows {
		result[r.Path] = HubTreeEntry{
			TreeEntry: TreeEntry{
				Path:   r.Path,
				Kind:   FileKind(r.Kind),
				Digest: Digest(r.Digest),
				Size:   r.Size,
				Mode:   r.Mode,
				MTime:  r.MTime,
			},
			Version: r.Version,
		}
	}
	return result, nil
}

// LookupEntry returns a single hub_tree entry by path.
func (s *ClientStore) LookupEntry(path string) (HubTreeEntry, bool, error) {
	var r hubTreeRow
	err := s.DB.Get(&r, `SELECT path, kind, digest, size, mode, mtime, version FROM hub_tree WHERE path = ?`, path)
	if err == sql.ErrNoRows {
		return HubTreeEntry{}, false, nil
	}
	if err != nil {
		return HubTreeEntry{}, false, err
	}
	return HubTreeEntry{
		TreeEntry: TreeEntry{
			Path:   r.Path,
			Kind:   FileKind(r.Kind),
			Digest: Digest(r.Digest),
			Size:   r.Size,
			Mode:   r.Mode,
			MTime:  r.MTime,
		},
		Version: r.Version,
	}, true, nil
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
		var digestBlob []byte
		if !digest.IsZero() {
			digestBlob = digest.Bytes()
		}
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO hub_tree (path, kind, digest, size, mode, mtime, version) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			path, int(kind), digestBlob, size, mode, mtime, version,
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
