package hubsync

import (
	"path/filepath"
	"testing"
)

func newTestHubStore(t *testing.T) *HubStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	store, cleanup, err := NewHubStore(HubStoreConfig{DBPath: dbPath, Hasher: sha256Hasher{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return store
}

func TestHubStoreAppendAndLatestVersion(t *testing.T) {
	store := newTestHubStore(t)

	// Empty store
	v, err := store.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Errorf("empty store version: got %d, want 0", v)
	}

	// Append a create
	digest := ComputeDigest([]byte("hello"))
	v1, err := store.Append(ChangeEntry{
		Path:   "README.md",
		Op:     OpCreate,
		Kind:   FileKindFile,
		Digest: digest,
		Size:   5,
		Mode:   0644,
		MTime:  1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v1 != 1 {
		t.Errorf("first version: got %d, want 1", v1)
	}

	v, _ = store.LatestVersion()
	if v != 1 {
		t.Errorf("latest version after 1 append: got %d, want 1", v)
	}

	// Append another
	v2, _ := store.Append(ChangeEntry{
		Path:   "src/main.go",
		Op:     OpCreate,
		Kind:   FileKindFile,
		Digest: ComputeDigest([]byte("package main")),
		Size:   12,
		Mode:   0644,
		MTime:  1001,
	})
	if v2 != 2 {
		t.Errorf("second version: got %d, want 2", v2)
	}
}

func TestHubStoreTreeSnapshot(t *testing.T) {
	store := newTestHubStore(t)

	digest1 := ComputeDigest([]byte("hello"))
	digest2 := ComputeDigest([]byte("world"))

	store.Append(ChangeEntry{Path: "a.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest1, Size: 5, MTime: 1000})
	store.Append(ChangeEntry{Path: "b.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest2, Size: 5, MTime: 1001})

	tree := store.TreeSnapshot()
	if len(tree) != 2 {
		t.Fatalf("tree size: got %d, want 2", len(tree))
	}
	if tree["a.txt"].Digest != digest1 {
		t.Error("a.txt digest mismatch")
	}
	if tree["b.txt"].Digest != digest2 {
		t.Error("b.txt digest mismatch")
	}
}

func TestHubStoreTreeUpdateAndDelete(t *testing.T) {
	store := newTestHubStore(t)

	digest1 := ComputeDigest([]byte("v1"))
	digest2 := ComputeDigest([]byte("v2"))

	// Create
	store.Append(ChangeEntry{Path: "file.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest1, Size: 2, MTime: 1000})

	// Update
	store.Append(ChangeEntry{Path: "file.txt", Op: OpUpdate, Kind: FileKindFile, Digest: digest2, Size: 2, MTime: 1001})

	tree := store.TreeSnapshot()
	if tree["file.txt"].Digest != digest2 {
		t.Error("expected updated digest")
	}

	// Delete
	store.Append(ChangeEntry{Path: "file.txt", Op: OpDelete, Kind: FileKindFile, MTime: 0})

	tree = store.TreeSnapshot()
	if _, exists := tree["file.txt"]; exists {
		t.Error("expected file.txt to be deleted from tree")
	}
}

func TestHubStorePathByDigest(t *testing.T) {
	store := newTestHubStore(t)

	digest := ComputeDigest([]byte("content"))
	store.Append(ChangeEntry{Path: "original.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest, Size: 7, MTime: 1000})

	path, ok := store.PathByDigest(digest)
	if !ok {
		t.Fatal("expected to find path by digest")
	}
	if path != "original.txt" {
		t.Errorf("path by digest: got %q, want %q", path, "original.txt")
	}

	// Non-existent digest
	_, ok = store.PathByDigest(ComputeDigest([]byte("nonexistent")))
	if ok {
		t.Error("expected no match for unknown digest")
	}
}

func TestHubStoreChangesSince(t *testing.T) {
	store := newTestHubStore(t)

	store.Append(ChangeEntry{Path: "a.txt", Op: OpCreate, Kind: FileKindFile, Digest: ComputeDigest([]byte("a")), Size: 1, MTime: 1000})
	store.Append(ChangeEntry{Path: "b.txt", Op: OpCreate, Kind: FileKindFile, Digest: ComputeDigest([]byte("b")), Size: 1, MTime: 1001})
	store.Append(ChangeEntry{Path: "c.txt", Op: OpCreate, Kind: FileKindFile, Digest: ComputeDigest([]byte("c")), Size: 1, MTime: 1002})

	// Since 0 -> all entries
	entries, err := store.ChangesSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("changes since 0: got %d, want 3", len(entries))
	}

	// Since 1 -> entries 2,3
	entries, _ = store.ChangesSince(1)
	if len(entries) != 2 {
		t.Fatalf("changes since 1: got %d, want 2", len(entries))
	}
	if entries[0].Path != "b.txt" {
		t.Errorf("first entry: got %q, want %q", entries[0].Path, "b.txt")
	}

	// Since 3 -> empty
	entries, _ = store.ChangesSince(3)
	if len(entries) != 0 {
		t.Fatalf("changes since 3: got %d, want 0", len(entries))
	}
}

func TestHubStoreTreeLookup(t *testing.T) {
	store := newTestHubStore(t)

	digest := ComputeDigest([]byte("hello"))
	store.Append(ChangeEntry{Path: "file.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest, Size: 5, Mode: 0644, MTime: 1000})

	entry, ok := store.TreeLookup("file.txt")
	if !ok {
		t.Fatal("expected to find file.txt")
	}
	if entry.Digest != digest {
		t.Error("digest mismatch")
	}
	if entry.Size != 5 {
		t.Errorf("size: got %d, want 5", entry.Size)
	}
	if entry.Mode != 0644 {
		t.Errorf("mode: got %o, want 0644", entry.Mode)
	}

	_, ok = store.TreeLookup("nonexistent.txt")
	if ok {
		t.Error("expected nonexistent.txt to not be found")
	}
}

func TestHubStoreLoadTreeOnReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")

	// First open: write some data
	store1, cleanup1, err := NewHubStore(HubStoreConfig{DBPath: dbPath, Hasher: sha256Hasher{}})
	if err != nil {
		t.Fatal(err)
	}
	digest := ComputeDigest([]byte("persistent"))
	store1.Append(ChangeEntry{Path: "saved.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest, Size: 10, MTime: 2000})
	cleanup1()

	// Reopen: tree should persist via hub_entry
	store2, cleanup2, err := NewHubStore(HubStoreConfig{DBPath: dbPath, Hasher: sha256Hasher{}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()

	tree := store2.TreeSnapshot()
	if len(tree) != 1 {
		t.Fatalf("reopened tree size: got %d, want 1", len(tree))
	}
	if tree["saved.txt"].Digest != digest {
		t.Error("reopened tree digest mismatch")
	}
}
