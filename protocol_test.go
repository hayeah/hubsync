package hubsync

import (
	"bytes"
	"io"
	"testing"
)

func TestWriteAndReadLengthPrefixed(t *testing.T) {
	msg := &SyncEvent{
		Version: 42,
		Path:    "test/file.txt",
		Event: &SyncEvent_Change{Change: &FileChange{
			Kind:   EntryKind_FILE,
			Digest: []byte{1, 2, 3},
			Size:   100,
			Mode:   0644,
			Mtime:  1000,
		}},
	}

	var buf bytes.Buffer
	if err := WriteLengthPrefixed(&buf, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read it back
	var got SyncEvent
	if err := ReadLengthPrefixed(&buf, &got); err != nil {
		t.Fatalf("read: %v", err)
	}

	if got.Version != 42 {
		t.Errorf("version: got %d, want 42", got.Version)
	}
	if got.Path != "test/file.txt" {
		t.Errorf("path: got %q, want %q", got.Path, "test/file.txt")
	}

	change := got.GetChange()
	if change == nil {
		t.Fatal("expected FileChange event")
	}
	if change.Size != 100 {
		t.Errorf("size: got %d, want 100", change.Size)
	}
}

func TestWriteAndReadMultipleMessages(t *testing.T) {
	var buf bytes.Buffer

	msgs := []*SyncEvent{
		{Version: 1, Path: "a.txt", Event: &SyncEvent_Change{Change: &FileChange{Size: 10}}},
		{Version: 2, Path: "b.txt", Event: &SyncEvent_Delete{Delete: &FileDelete{}}},
		{Version: 3, Path: "c.txt", Event: &SyncEvent_Change{Change: &FileChange{Size: 30}}},
	}

	for _, msg := range msgs {
		if err := WriteLengthPrefixed(&buf, msg); err != nil {
			t.Fatal(err)
		}
	}

	for i, expected := range msgs {
		var got SyncEvent
		if err := ReadLengthPrefixed(&buf, &got); err != nil {
			t.Fatalf("read message %d: %v", i, err)
		}
		if got.Version != expected.Version {
			t.Errorf("msg %d version: got %d, want %d", i, got.Version, expected.Version)
		}
		if got.Path != expected.Path {
			t.Errorf("msg %d path: got %q, want %q", i, got.Path, expected.Path)
		}
	}

	// Should be EOF
	var extra SyncEvent
	err := ReadLengthPrefixed(&buf, &extra)
	if err != io.EOF {
		t.Errorf("expected EOF, got: %v", err)
	}
}

func TestReadLengthPrefixedDeleteEvent(t *testing.T) {
	var buf bytes.Buffer

	msg := &SyncEvent{
		Version: 5,
		Path:    "deleted.txt",
		Event:   &SyncEvent_Delete{Delete: &FileDelete{}},
	}

	WriteLengthPrefixed(&buf, msg)

	var got SyncEvent
	ReadLengthPrefixed(&buf, &got)

	if got.GetDelete() == nil {
		t.Error("expected FileDelete event")
	}
	if got.GetChange() != nil {
		t.Error("should not have FileChange")
	}
}

func TestReadLengthPrefixedInlineData(t *testing.T) {
	var buf bytes.Buffer

	data := []byte("inline content here")
	msg := &SyncEvent{
		Version: 1,
		Path:    "small.txt",
		Event: &SyncEvent_Change{Change: &FileChange{
			Kind: EntryKind_FILE,
			Size: uint64(len(data)),
			Data: data,
		}},
	}

	WriteLengthPrefixed(&buf, msg)

	var got SyncEvent
	ReadLengthPrefixed(&buf, &got)

	change := got.GetChange()
	if change == nil {
		t.Fatal("expected FileChange")
	}
	if !bytes.Equal(change.Data, data) {
		t.Errorf("inline data: got %q, want %q", change.Data, data)
	}
}
