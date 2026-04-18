// Package hubsync implements a read-only file synchronization system.
// A hub watches a directory and serves changes; clients subscribe and replicate.
package hubsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Digest is a content hash stored as raw bytes. Length depends on the hub's
// configured [hub] hash algorithm: 16 bytes for xxh128, 32 bytes for sha256.
//
// Represented as string so it's immutable, comparable, and usable as a map
// key with no allocation. Convert to/from []byte with `Digest(b)` and
// `[]byte(d)` (SQLite BLOBs round-trip as []byte).
type Digest string

// NewDigest copies b into a Digest. The returned Digest aliases no memory
// with b (string conversion copies).
func NewDigest(b []byte) Digest { return Digest(b) }

// Bytes returns the digest as []byte. The result must not be mutated.
func (d Digest) Bytes() []byte { return []byte(d) }

// Hex returns the lowercase hex encoding of the digest.
func (d Digest) Hex() string { return hex.EncodeToString([]byte(d)) }

// String implements fmt.Stringer.
func (d Digest) String() string { return d.Hex() }

// ParseDigest decodes a hex string into a Digest. Accepts any even-length
// hex input; callers that need a specific algo length should check Size().
func ParseDigest(s string) (Digest, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("invalid digest hex: %w", err)
	}
	return Digest(b), nil
}

// IsZero reports whether the digest is empty (unset).
func (d Digest) IsZero() bool { return len(d) == 0 }

// Size returns the digest's byte length.
func (d Digest) Size() int { return len(d) }

// MarshalJSON emits the digest as a lowercase hex string (raw bytes
// would round-trip through JSON's base64 default, which doesn't match
// how operators read the column in `sqlite3` via `hex(digest)`).
// Empty digest marshals as `""` so it plays nicely with `omitempty`.
func (d Digest) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Hex())
}

// UnmarshalJSON accepts a hex-encoded string and stores the raw bytes.
// Empty string decodes to the zero Digest. Any non-hex input is an
// error; callers that want to tolerate legacy payloads should
// pre-validate.
func (d *Digest) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*d = ""
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("Digest.UnmarshalJSON: %w", err)
	}
	*d = Digest(b)
	return nil
}

// SHA256Digest returns the SHA-256 digest of data. Test/fixture helper; in
// production code paths, use a configured Hasher instead.
func SHA256Digest(data []byte) Digest {
	s := sha256.Sum256(data)
	return Digest(s[:])
}

// ComputeDigest is a deprecated alias for SHA256Digest kept for in-tree
// tests. Production code paths should use a configured Hasher.
//
// Deprecated: use SHA256Digest or a Hasher.
func ComputeDigest(data []byte) Digest { return SHA256Digest(data) }

// FileKind describes the type of a filesystem entry.
// Maps to the protobuf FileKind enum.
type FileKind int

const (
	FileKindFile      FileKind = 0
	FileKindDirectory FileKind = 1
	FileKindSymlink   FileKind = 2
)

// Label returns the wire-format label for k (`"file"` / `"directory"`
// / `"symlink"`, or `""` for an unknown int).
func (k FileKind) Label() string {
	switch k {
	case FileKindFile:
		return "file"
	case FileKindDirectory:
		return "directory"
	case FileKindSymlink:
		return "symlink"
	default:
		return ""
	}
}

// MarshalJSON emits the label rather than the raw int — the SQL
// column is an int enum but every wire consumer reads the label.
func (k FileKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.Label())
}

// UnmarshalJSON accepts any of `"file"` / `"directory"` / `"symlink"`.
// Unknown labels are a decode error.
func (k *FileKind) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "file":
		*k = FileKindFile
	case "directory":
		*k = FileKindDirectory
	case "symlink":
		*k = FileKindSymlink
	default:
		return fmt.Errorf("FileKind.UnmarshalJSON: unknown label %q", s)
	}
	return nil
}

// ChangeEntry represents a single mutation in the change log.
type ChangeEntry struct {
	Version int64
	Path    string
	Op      ChangeOp
	Kind    FileKind
	Digest  Digest
	Size    int64
	Mode    uint32
	MTime   int64 // unix seconds
}

// ChangeOp describes the type of change.
type ChangeOp string

const (
	OpCreate ChangeOp = "create"
	OpUpdate ChangeOp = "update"
	OpDelete ChangeOp = "delete"
)

// TreeEntry represents the current state of a file in the materialized tree.
type TreeEntry struct {
	Path   string
	Kind   FileKind
	Digest Digest
	Size   int64
	Mode   uint32
	MTime  int64
}

// InlineThreshold is the max file size to inline in sync events.
const InlineThreshold = 64 * 1024 // 64KB
