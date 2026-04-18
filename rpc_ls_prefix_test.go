package hubsync

import (
	"testing"
)

func TestDelimitedEntries_TopLevel(t *testing.T) {
	rows := []HubEntry{
		{Path: "a.txt", Kind: FileKindFile},
		{Path: "b.txt", Kind: FileKindFile},
		{Path: "sub", Kind: FileKindDirectory},
		{Path: "sub/c.txt", Kind: FileKindFile},
		{Path: "sub/deeper/d.txt", Kind: FileKindFile},
	}
	got := DelimitedEntries(rows, "")
	want := []string{"a.txt", "b.txt", "sub", "sub/"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want=%d: %+v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.Path != want[i] {
			t.Errorf("[%d] path=%q want %q", i, e.Path, want[i])
		}
	}
	// Synthetic dir "sub/" carries Kind=Directory (same as real dir row).
	if got[3].Kind != FileKindDirectory {
		t.Errorf("synthetic dir kind=%v want directory", got[3].Kind)
	}
}

func TestDelimitedEntries_UnderPrefix(t *testing.T) {
	rows := []HubEntry{
		{Path: "sub", Kind: FileKindDirectory},
		{Path: "sub/a.txt", Kind: FileKindFile},
		{Path: "sub/b.txt", Kind: FileKindFile},
		{Path: "sub/deeper/d.txt", Kind: FileKindFile},
		{Path: "sub/deeper/e.txt", Kind: FileKindFile},
	}
	got := DelimitedEntries(rows, "sub/")
	want := []string{"sub/a.txt", "sub/b.txt", "sub/deeper/"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want=%d: %+v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.Path != want[i] {
			t.Errorf("[%d] path=%q want %q", i, e.Path, want[i])
		}
	}
}

func TestDelimitedEntries_EmptyPrefixExact(t *testing.T) {
	// A row at exactly the prefix level (e.g. dir row "sub") must not be
	// emitted when listing "sub/" — its children are what the caller wants.
	rows := []HubEntry{
		{Path: "sub", Kind: FileKindDirectory},
		{Path: "sub/a.txt", Kind: FileKindFile},
	}
	got := DelimitedEntries(rows, "sub/")
	if len(got) != 1 || got[0].Path != "sub/a.txt" {
		t.Fatalf("got %+v", got)
	}
}

func TestDelimitedEntries_PrefixOutsideTree(t *testing.T) {
	rows := []HubEntry{
		{Path: "a.txt", Kind: FileKindFile},
	}
	got := DelimitedEntries(rows, "zzz/")
	if len(got) != 0 {
		t.Fatalf("expected no entries, got %+v", got)
	}
}

func TestEntriesByPrefix_Empty(t *testing.T) {
	store := newTestHubStoreForPrefix(t)
	got, err := store.EntriesByPrefix("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("empty prefix should return every row, got %d: %+v", len(got), got)
	}
}

func TestEntriesByPrefix_DirPrefix(t *testing.T) {
	store := newTestHubStoreForPrefix(t)
	got, err := store.EntriesByPrefix("sub/")
	if err != nil {
		t.Fatal(err)
	}
	// Expect 2 rows: "sub" (the dir row, matches `path = "sub"`) and "sub/a.txt".
	paths := map[string]bool{}
	for _, r := range got {
		paths[r.Path] = true
	}
	if !paths["sub"] || !paths["sub/a.txt"] {
		t.Errorf("want {sub, sub/a.txt}, got %v", paths)
	}
	if paths["a.txt"] {
		t.Errorf("sibling a.txt should not leak into sub/ query")
	}
}

func newTestHubStoreForPrefix(t *testing.T) *HubStore {
	t.Helper()
	store := newTestHubStore(t)
	digest := ComputeDigest([]byte("x"))
	store.Append(ChangeEntry{Path: "a.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest, Size: 1, MTime: 1000})
	store.Append(ChangeEntry{Path: "sub", Op: OpCreate, Kind: FileKindDirectory, Mode: 0755, MTime: 1001})
	store.Append(ChangeEntry{Path: "sub/a.txt", Op: OpCreate, Kind: FileKindFile, Digest: digest, Size: 1, MTime: 1002})
	return store
}
