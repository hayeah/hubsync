// Package hubsync implements a read-only file synchronization system.
// A hub watches a directory and serves changes; clients subscribe and replicate.
package hubsync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Digest is a SHA-256 content hash stored as raw bytes.
type Digest [sha256.Size]byte

// ComputeDigest returns the SHA-256 digest of data.
func ComputeDigest(data []byte) Digest {
	return sha256.Sum256(data)
}

// Hex returns the lowercase hex encoding of the digest.
func (d Digest) Hex() string {
	return hex.EncodeToString(d[:])
}

// String implements fmt.Stringer.
func (d Digest) String() string {
	return d.Hex()
}

// ParseDigest decodes a hex string into a Digest.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	b, err := hex.DecodeString(s)
	if err != nil {
		return d, fmt.Errorf("invalid digest hex: %w", err)
	}
	if len(b) != sha256.Size {
		return d, fmt.Errorf("invalid digest length: got %d, want %d", len(b), sha256.Size)
	}
	copy(d[:], b)
	return d, nil
}

// IsZero reports whether the digest is all zeros (unset).
func (d Digest) IsZero() bool {
	return d == Digest{}
}

// FileKind describes the type of a filesystem entry.
// Maps to the protobuf FileKind enum.
type FileKind int

const (
	FileKindFile      FileKind = 0
	FileKindDirectory FileKind = 1
	FileKindSymlink   FileKind = 2
)

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
