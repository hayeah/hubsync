package hubsync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScannerScanAll(t *testing.T) {
	dir := t.TempDir()

	// Create test files
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world"), 0644)

	scanner := NewScanner(ScannerConfig{WatchDir: dir, Hasher: sha256Hasher{}})
	result, err := scanner.ScanAll(nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 2 {
		t.Fatalf("scan result count: got %d, want 2", len(result))
	}

	a, ok := result["a.txt"]
	if !ok {
		t.Fatal("a.txt not found in scan")
	}
	if a.Digest != ComputeDigest([]byte("hello")) {
		t.Error("a.txt digest mismatch")
	}
	if a.Size != 5 {
		t.Errorf("a.txt size: got %d, want 5", a.Size)
	}

	b, ok := result["sub/b.txt"]
	if !ok {
		t.Fatal("sub/b.txt not found in scan")
	}
	if b.Digest != ComputeDigest([]byte("world")) {
		t.Error("sub/b.txt digest mismatch")
	}
}

func TestScannerScanAllDigestCache(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cached.txt"), []byte("content"), 0644)

	scanner := NewScanner(ScannerConfig{WatchDir: dir, Hasher: sha256Hasher{}})

	// First scan: compute digest
	result1, _ := scanner.ScanAll(nil)
	info1 := result1["cached.txt"]

	// Build a "current tree" with matching mtime+size but a fake digest
	fakeDigest := ComputeDigest([]byte("fake"))
	currentTree := map[string]TreeEntry{
		"cached.txt": {
			Path:   "cached.txt",
			Digest: fakeDigest,
			Size:   info1.Size,
			MTime:  info1.MTime,
		},
	}

	// Second scan with cache: should reuse the fake digest (not re-read file)
	result2, _ := scanner.ScanAll(currentTree)
	if result2["cached.txt"].Digest != fakeDigest {
		t.Error("expected cached digest to be reused when mtime+size match")
	}
}

func TestScannerScanAllCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("original"), 0644)

	scanner := NewScanner(ScannerConfig{WatchDir: dir, Hasher: sha256Hasher{}})
	result1, _ := scanner.ScanAll(nil)
	info1 := result1["file.txt"]

	// Modify the file (different size triggers cache miss)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("modified-content"), 0644)

	currentTree := map[string]TreeEntry{
		"file.txt": {
			Path:   "file.txt",
			Digest: info1.Digest,
			Size:   info1.Size,
			MTime:  info1.MTime,
		},
	}

	result2, _ := scanner.ScanAll(currentTree)
	expected := ComputeDigest([]byte("modified-content"))
	if result2["file.txt"].Digest != expected {
		t.Error("expected fresh digest after file modification")
	}
}

func TestScannerDiffCreates(t *testing.T) {
	scanner := NewScanner(ScannerConfig{Hasher: sha256Hasher{}}) // dir not used by Diff

	digest := ComputeDigest([]byte("new"))
	scanned := map[string]FileInfo{
		"new.txt": {Path: "new.txt", Digest: digest, Size: 3, MTime: 1000},
	}

	changes := scanner.Diff(scanned, nil)
	if len(changes) != 1 {
		t.Fatalf("diff changes: got %d, want 1", len(changes))
	}
	if changes[0].Op != OpCreate {
		t.Errorf("op: got %q, want %q", changes[0].Op, OpCreate)
	}
	if changes[0].Path != "new.txt" {
		t.Errorf("path: got %q, want %q", changes[0].Path, "new.txt")
	}
}

func TestScannerDiffUpdates(t *testing.T) {
	scanner := NewScanner(ScannerConfig{Hasher: sha256Hasher{}})

	oldDigest := ComputeDigest([]byte("old"))
	newDigest := ComputeDigest([]byte("new"))

	currentTree := map[string]TreeEntry{
		"file.txt": {Path: "file.txt", Digest: oldDigest, Size: 3},
	}
	scanned := map[string]FileInfo{
		"file.txt": {Path: "file.txt", Digest: newDigest, Size: 3, MTime: 1000},
	}

	changes := scanner.Diff(scanned, currentTree)
	if len(changes) != 1 {
		t.Fatalf("diff changes: got %d, want 1", len(changes))
	}
	if changes[0].Op != OpUpdate {
		t.Errorf("op: got %q, want %q", changes[0].Op, OpUpdate)
	}
}

func TestScannerDiffDeletes(t *testing.T) {
	scanner := NewScanner(ScannerConfig{Hasher: sha256Hasher{}})

	currentTree := map[string]TreeEntry{
		"gone.txt": {Path: "gone.txt", Digest: ComputeDigest([]byte("x")), Size: 1},
	}
	scanned := map[string]FileInfo{} // file no longer exists

	changes := scanner.Diff(scanned, currentTree)
	if len(changes) != 1 {
		t.Fatalf("diff changes: got %d, want 1", len(changes))
	}
	if changes[0].Op != OpDelete {
		t.Errorf("op: got %q, want %q", changes[0].Op, OpDelete)
	}
}

func TestScannerDiffNoChanges(t *testing.T) {
	scanner := NewScanner(ScannerConfig{Hasher: sha256Hasher{}})

	digest := ComputeDigest([]byte("same"))
	currentTree := map[string]TreeEntry{
		"file.txt": {Path: "file.txt", Digest: digest, Size: 4},
	}
	scanned := map[string]FileInfo{
		"file.txt": {Path: "file.txt", Digest: digest, Size: 4, MTime: 1000},
	}

	changes := scanner.Diff(scanned, currentTree)
	if len(changes) != 0 {
		t.Errorf("expected no changes, got %d", len(changes))
	}
}

func TestScannerDiffCreatesBeforeDeletes(t *testing.T) {
	scanner := NewScanner(ScannerConfig{Hasher: sha256Hasher{}})

	digest := ComputeDigest([]byte("moved"))

	// Simulates a rename: old path deleted, new path created
	currentTree := map[string]TreeEntry{
		"old.txt": {Path: "old.txt", Digest: digest, Size: 5},
	}
	scanned := map[string]FileInfo{
		"new.txt": {Path: "new.txt", Digest: digest, Size: 5, MTime: 1000},
	}

	changes := scanner.Diff(scanned, currentTree)
	if len(changes) != 2 {
		t.Fatalf("diff changes: got %d, want 2", len(changes))
	}
	// Creates must come before deletes (spec requirement for move dedup)
	if changes[0].Op != OpCreate {
		t.Errorf("first change should be create, got %q", changes[0].Op)
	}
	if changes[1].Op != OpDelete {
		t.Errorf("second change should be delete, got %q", changes[1].Op)
	}
}
