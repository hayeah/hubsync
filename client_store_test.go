package hubsync

import (
	"path/filepath"
	"testing"
)

func newTestClientStore(t *testing.T) *ClientStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "client.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewClientStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestClientStoreHubVersion(t *testing.T) {
	store := newTestClientStore(t)

	// Default: 0
	v, err := store.HubVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Errorf("initial version: got %d, want 0", v)
	}

	// Set and read
	store.SetHubVersion(42)
	v, _ = store.HubVersion()
	if v != 42 {
		t.Errorf("after set: got %d, want 42", v)
	}

	// Overwrite
	store.SetHubVersion(100)
	v, _ = store.HubVersion()
	if v != 100 {
		t.Errorf("after overwrite: got %d, want 100", v)
	}
}

func TestClientStoreUpsertAndDelete(t *testing.T) {
	store := newTestClientStore(t)
	digest := ComputeDigest([]byte("hello"))

	// Insert
	err := store.UpsertEntry("file.txt", FileKindFile, digest, 5, 0644, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Verify via LookupByDigest
	path, found, err := store.LookupByDigest(digest, "")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected to find entry by digest")
	}
	if path != "file.txt" {
		t.Errorf("path: got %q, want %q", path, "file.txt")
	}

	// Update (upsert same path, different digest)
	digest2 := ComputeDigest([]byte("updated"))
	store.UpsertEntry("file.txt", FileKindFile, digest2, 7, 0644, 1001, 2)

	// Old digest should not match
	_, found, _ = store.LookupByDigest(digest, "")
	if found {
		t.Error("old digest should no longer match")
	}

	// New digest should match
	path, found, _ = store.LookupByDigest(digest2, "")
	if !found || path != "file.txt" {
		t.Error("new digest should match file.txt")
	}

	// Delete
	store.DeleteEntry("file.txt")
	_, found, _ = store.LookupByDigest(digest2, "")
	if found {
		t.Error("deleted entry should not be found")
	}
}

func TestClientStoreLookupByDigestExclude(t *testing.T) {
	store := newTestClientStore(t)
	digest := ComputeDigest([]byte("shared"))

	store.UpsertEntry("a.txt", FileKindFile, digest, 6, 0644, 1000, 1)
	store.UpsertEntry("b.txt", FileKindFile, digest, 6, 0644, 1000, 1)

	// Exclude a.txt -> should find b.txt
	path, found, _ := store.LookupByDigest(digest, "a.txt")
	if !found {
		t.Fatal("expected to find another path with same digest")
	}
	if path != "b.txt" {
		t.Errorf("expected b.txt, got %q", path)
	}

	// Exclude b.txt -> should find a.txt
	path, found, _ = store.LookupByDigest(digest, "b.txt")
	if !found || path != "a.txt" {
		t.Errorf("expected a.txt, got %q (found=%v)", path, found)
	}
}

func TestClientStoreApplyChange(t *testing.T) {
	store := newTestClientStore(t)
	digest := ComputeDigest([]byte("content"))

	// Apply create
	err := store.ApplyChange(1, "file.txt", OpCreate, FileKindFile, digest, 7, 0644, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Version should be updated
	v, _ := store.HubVersion()
	if v != 1 {
		t.Errorf("version after apply: got %d, want 1", v)
	}

	// Entry should exist
	path, found, _ := store.LookupByDigest(digest, "")
	if !found || path != "file.txt" {
		t.Error("entry should exist after apply create")
	}

	// Apply delete
	err = store.ApplyChange(2, "file.txt", OpDelete, FileKindFile, Digest{}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	v, _ = store.HubVersion()
	if v != 2 {
		t.Errorf("version after delete: got %d, want 2", v)
	}

	_, found, _ = store.LookupByDigest(digest, "")
	if found {
		t.Error("entry should not exist after apply delete")
	}
}

func TestClientStoreApplyChangeTransaction(t *testing.T) {
	store := newTestClientStore(t)

	// Apply multiple changes — each should atomically update hub_tree + sync_state
	d1 := ComputeDigest([]byte("a"))
	d2 := ComputeDigest([]byte("b"))

	store.ApplyChange(1, "a.txt", OpCreate, FileKindFile, d1, 1, 0644, 1000)
	store.ApplyChange(2, "b.txt", OpCreate, FileKindFile, d2, 1, 0644, 1001)

	v, _ := store.HubVersion()
	if v != 2 {
		t.Errorf("version: got %d, want 2", v)
	}

	// Both entries should exist
	_, found1, _ := store.LookupByDigest(d1, "")
	_, found2, _ := store.LookupByDigest(d2, "")
	if !found1 || !found2 {
		t.Error("both entries should exist")
	}
}
