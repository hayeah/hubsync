package hubsync

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDigestMarshalJSON covers hex serialization and round-trip. The
// ambient encoding across the repo (sqlite3 shell, README examples,
// duckql queries) is hex, so Digest must emit hex even though its Go
// representation is a raw-byte string.
func TestDigestMarshalJSON(t *testing.T) {
	raw := []byte{0x01, 0x02, 0xab, 0xff}
	d := Digest(raw)

	// Marshal → hex string.
	got, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != `"0102abff"` {
		t.Errorf("marshal = %s, want %q", got, `"0102abff"`)
	}

	// Unmarshal round-trip.
	var back Digest
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back) != string(raw) {
		t.Errorf("round-trip bytes mismatch: got %x, want %x", []byte(back), raw)
	}

	// Empty Digest round-trips as empty string.
	empty, err := json.Marshal(Digest(""))
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if string(empty) != `""` {
		t.Errorf("empty marshal = %s, want %q", empty, `""`)
	}

	// omitempty works: empty digest disappears from a wrapping struct.
	type wrap struct {
		D Digest `json:"d,omitempty"`
	}
	w, err := json.Marshal(wrap{})
	if err != nil {
		t.Fatalf("marshal wrap: %v", err)
	}
	if string(w) != `{}` {
		t.Errorf("omitempty failed: %s", w)
	}

	// Non-hex input is a decode error.
	var bad Digest
	if err := json.Unmarshal([]byte(`"not-hex!"`), &bad); err == nil {
		t.Errorf("expected decode error for non-hex, got Digest(%x)", []byte(bad))
	}
}

// TestFileKindMarshalJSON covers label serialization, round-trip, and
// rejection of unknown labels.
func TestFileKindMarshalJSON(t *testing.T) {
	cases := []struct {
		kind  FileKind
		label string
	}{
		{FileKindFile, `"file"`},
		{FileKindDirectory, `"directory"`},
		{FileKindSymlink, `"symlink"`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.kind)
		if err != nil {
			t.Fatalf("marshal %v: %v", c.kind, err)
		}
		if string(got) != c.label {
			t.Errorf("marshal %v = %s, want %s", c.kind, got, c.label)
		}
		var back FileKind
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", got, err)
		}
		if back != c.kind {
			t.Errorf("round-trip %v: got %v", c.kind, back)
		}
	}

	// Unknown label is a decode error.
	var bad FileKind
	if err := json.Unmarshal([]byte(`"doorknob"`), &bad); err == nil {
		t.Errorf("expected error for unknown label, got %v", bad)
	}
}

// TestHubEntryMarshalShape asserts the full JSON shape of HubEntry is
// stable — every hub_entry column maps to the documented wire field.
// Catches accidental renames / additions that would break
// `duckql`-style queries downstream.
func TestHubEntryMarshalShape(t *testing.T) {
	e := HubEntry{
		Path:              "dir/a.txt",
		Kind:              FileKindFile,
		Digest:            Digest([]byte{0xde, 0xad, 0xbe, 0xef}),
		Size:              42,
		Mode:              0o644,
		MTime:             1700000000,
		Version:           17,
		ArchiveState:      ArchiveStateArchived,
		ArchiveFileID:     "fake-file-id",
		ArchiveSHA1:       Digest([]byte{0xab, 0xcd}),
		ArchiveUploadedAt: 1700000001234,
		UpdatedAt:         1700000002,
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Fields we expect (order-insensitive; we just check containment).
	wantFragments := []string{
		`"path":"dir/a.txt"`,
		`"kind":"file"`,
		`"digest":"deadbeef"`,
		`"size":42`,
		`"mode":420`, // 0o644 decimal
		`"mtime":1700000000`,
		`"version":17`,
		`"archive_state":"archived"`,
		`"archive_file_id":"fake-file-id"`,
		`"archive_sha1":"abcd"`,
		`"archive_uploaded_at":1700000001234`,
		`"updated_at":1700000002`,
	}
	for _, frag := range wantFragments {
		if !strings.Contains(string(got), frag) {
			t.Errorf("HubEntry JSON missing fragment %s\nfull: %s", frag, got)
		}
	}

	// Round-trip: decode back and compare.
	var back HubEntry
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != e {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", back, e)
	}

	// Optional fields omit when zero: empty digest / sha1 / file_id /
	// uploaded_at should all disappear from a NULL-archive row.
	empty := HubEntry{
		Path:         "empty",
		Kind:         FileKindFile,
		Size:         0,
		Mode:         0,
		MTime:        1,
		Version:      1,
		ArchiveState: "",
		UpdatedAt:    2,
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	for _, forbidden := range []string{"digest", "archive_file_id", "archive_sha1", "archive_uploaded_at"} {
		if strings.Contains(string(raw), `"`+forbidden+`"`) {
			t.Errorf("NULL-archive row leaked %q: %s", forbidden, raw)
		}
	}
	// But non-optional fields stay, even at zero.
	for _, required := range []string{`"mode":0`, `"size":0`, `"archive_state":""`} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("NULL-archive row missing %q: %s", required, raw)
		}
	}
}
