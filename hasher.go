package hubsync

import (
	"crypto/sha256"
	"fmt"
	"hash"

	"github.com/zeebo/xxh3"
)

// Hasher abstracts the configured content-hash algorithm.
// Two implementations exist: xxh128 (default, 16 bytes) and sha256 (32 bytes).
type Hasher interface {
	Name() string   // "xxh128" or "sha256"
	Size() int      // digest byte length
	New() hash.Hash // streaming; Sum(nil) yields the raw digest bytes
	Sum(data []byte) Digest
}

// NewHasher returns the Hasher for the given algorithm name.
func NewHasher(name string) (Hasher, error) {
	switch name {
	case HashXXH128:
		return xxh128Hasher{}, nil
	case HashSHA256:
		return sha256Hasher{}, nil
	default:
		return nil, fmt.Errorf("unknown hash algorithm: %q", name)
	}
}

type xxh128Hasher struct{}

func (xxh128Hasher) Name() string   { return HashXXH128 }
func (xxh128Hasher) Size() int      { return 16 }
func (xxh128Hasher) New() hash.Hash { return &xxh128Stream{h: xxh3.New()} }
func (xxh128Hasher) Sum(data []byte) Digest {
	u := xxh3.Hash128(data).Bytes()
	return Digest(u[:])
}

type sha256Hasher struct{}

func (sha256Hasher) Name() string   { return HashSHA256 }
func (sha256Hasher) Size() int      { return sha256.Size }
func (sha256Hasher) New() hash.Hash { return sha256.New() }
func (sha256Hasher) Sum(data []byte) Digest {
	s := sha256.Sum256(data)
	return Digest(s[:])
}

// xxh128Stream adapts *xxh3.Hasher to hash.Hash with 16-byte Sum output.
// The underlying Hasher's Sum returns only the 64-bit half; we need both halves.
type xxh128Stream struct {
	h *xxh3.Hasher
}

func (s *xxh128Stream) Write(p []byte) (int, error) { return s.h.Write(p) }
func (s *xxh128Stream) Reset()                      { s.h.Reset() }
func (s *xxh128Stream) BlockSize() int              { return s.h.BlockSize() }
func (s *xxh128Stream) Size() int                   { return 16 }
func (s *xxh128Stream) Sum(b []byte) []byte {
	u := s.h.Sum128().Bytes()
	return append(b, u[:]...)
}
