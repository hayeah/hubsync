package hubsync

import (
	"os"
)

// generateClientDB creates a SQLite DB with hub_tree and sync_state (populated
// with hub_version + hash_algo) for /snapshots-tree/{version} responses.
func generateClientDB(store *HubStore, version int64, hashAlgo string) ([]byte, error) {
	tmpFile, err := os.CreateTemp("", "hubsync-snapshot-*.db")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	db, err := OpenDB(tmpPath)
	if err != nil {
		return nil, err
	}

	clientStore, err := NewClientStore(db)
	if err != nil {
		db.Close()
		return nil, err
	}

	tree := store.TreeSnapshot()
	for path, entry := range tree {
		if err := clientStore.UpsertEntry(path, entry.Kind, entry.Digest, entry.Size, entry.Mode, entry.MTime, version); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := clientStore.SetHubVersion(version); err != nil {
		db.Close()
		return nil, err
	}
	if hashAlgo != "" {
		if err := clientStore.SetHashAlgo(hashAlgo); err != nil {
			db.Close()
			return nil, err
		}
	}

	db.Close()
	return readAndCheckpoint(tmpPath)
}

func readAndCheckpoint(path string) ([]byte, error) {
	db, err := OpenDB(path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	db.Close()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
