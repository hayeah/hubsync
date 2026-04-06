package hubsync

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// GenerateSnapshot creates a zstd-compressed tarball containing:
// - .hubsync/hub_tree.db: SQLite DB with hub_tree + sync_state tables
// - All files from the current tree
func GenerateSnapshot(store *HubStore, dir string, version int64, w io.Writer) error {
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("create zstd writer: %w", err)
	}
	defer zw.Close()

	tw := tar.NewWriter(zw)
	defer tw.Close()

	// Generate the client DB
	dbData, err := generateClientDB(store, version)
	if err != nil {
		return fmt.Errorf("generate client db: %w", err)
	}

	// Write the DB to the tarball
	if err := tw.WriteHeader(&tar.Header{
		Name: ".hubsync/hub_tree.db",
		Size: int64(len(dbData)),
		Mode: 0644,
	}); err != nil {
		return err
	}
	if _, err := tw.Write(dbData); err != nil {
		return err
	}

	// Write all files from the current tree
	tree := store.TreeSnapshot()
	for path, entry := range tree {
		if entry.Kind != FileKindFile {
			continue
		}
		fullPath := filepath.Join(dir, path)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue // skip files that can't be read
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: path,
			Size: int64(len(data)),
			Mode: int64(entry.Mode),
		}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}

	return nil
}

// generateClientDB creates an in-memory SQLite DB with hub_tree and sync_state.
func generateClientDB(store *HubStore, version int64) ([]byte, error) {
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

	db.Close()

	// Force WAL checkpoint and read
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

// ExtractSnapshot extracts a snapshot tarball to the given directory.
// Returns the path to the extracted hub_tree.db.
func ExtractSnapshot(r io.Reader, dir string) (string, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("create zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var dbPath string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar: %w", err)
		}

		target := filepath.Join(dir, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return "", err
			}
			f, err := os.Create(target)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return "", err
			}
			f.Close()

			if hdr.Name == ".hubsync/hub_tree.db" {
				dbPath = target
			}
		}
	}

	return dbPath, nil
}
