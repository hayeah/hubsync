package hubsync

import (
	"crypto/sha256"
	"math"
)

const (
	minBlockSize     = 1024      // 1KB
	maxBlockSize     = 64 * 1024 // 64KB
	defaultBlockSize = 8 * 1024  // 8KB
)

// OptimalBlockSize computes the rsync-optimal block size for a file.
// Uses the rsync thesis heuristic: sqrt(24 * fileSize), clamped to [1KB, 64KB].
func OptimalBlockSize(fileSize int64) int {
	if fileSize <= 0 {
		return defaultBlockSize
	}
	bs := int(math.Sqrt(24.0 * float64(fileSize)))
	if bs < minBlockSize {
		return minBlockSize
	}
	if bs > maxBlockSize {
		return maxBlockSize
	}
	return bs
}

// BlockSig holds the weak and strong hash for a single block.
type BlockSig struct {
	Index      uint32
	WeakHash   uint32
	StrongHash [sha256.Size]byte
}

// ComputeSignature divides data into fixed-size blocks and computes
// a weak rolling checksum + SHA-256 strong hash for each block.
func ComputeSignature(data []byte, blockSize int) []BlockSig {
	if blockSize <= 0 {
		blockSize = defaultBlockSize
	}
	nblocks := (len(data) + blockSize - 1) / blockSize
	sigs := make([]BlockSig, 0, nblocks)

	for i := 0; i < len(data); i += blockSize {
		end := i + blockSize
		if end > len(data) {
			end = len(data)
		}
		block := data[i:end]

		sigs = append(sigs, BlockSig{
			Index:      uint32(len(sigs)),
			WeakHash:   weakHash(block),
			StrongHash: sha256.Sum256(block),
		})
	}
	return sigs
}

// DeltaOps represents the operations to reconstruct a target file from
// a base file. Each op is either a CopyBlock (reuse a base block by index)
// or a DataBlock (literal new bytes).
type DeltaOps struct {
	Ops        []deltaOp
	TargetSize int64
}

type deltaOp struct {
	IsCopy    bool
	CopyIndex uint32
	Data      []byte
}

// ComputeDelta scans the target file and produces delta ops referencing
// blocks from the base file (identified by their signatures).
func ComputeDelta(target []byte, sigs []BlockSig, blockSize int) DeltaOps {
	if blockSize <= 0 {
		blockSize = defaultBlockSize
	}

	// Build weak hash -> signatures lookup
	weakMap := make(map[uint32][]BlockSig, len(sigs))
	for _, sig := range sigs {
		weakMap[sig.WeakHash] = append(weakMap[sig.WeakHash], sig)
	}

	result := DeltaOps{TargetSize: int64(len(target))}
	if len(target) == 0 || len(sigs) == 0 {
		if len(target) > 0 {
			result.Ops = append(result.Ops, deltaOp{Data: target})
		}
		return result
	}

	// Sliding window over target
	var pending []byte // unmatched bytes waiting to be flushed
	pos := 0

	for pos <= len(target)-blockSize {
		window := target[pos : pos+blockSize]
		wh := weakHash(window)

		if candidates, ok := weakMap[wh]; ok {
			// Weak match — verify with strong hash
			sh := sha256.Sum256(window)
			matched := false
			for _, sig := range candidates {
				if sig.StrongHash == sh {
					// Match! Flush pending data, then emit copy op
					if len(pending) > 0 {
						result.Ops = append(result.Ops, deltaOp{Data: pending})
						pending = nil
					}
					result.Ops = append(result.Ops, deltaOp{IsCopy: true, CopyIndex: sig.Index})
					pos += blockSize
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}

		// No match — advance by 1 byte
		pending = append(pending, target[pos])
		pos++
	}

	// Remaining bytes (tail shorter than blockSize)
	if pos < len(target) {
		pending = append(pending, target[pos:]...)
	}

	// Flush any remaining pending data
	if len(pending) > 0 {
		result.Ops = append(result.Ops, deltaOp{Data: pending})
	}

	return result
}

// ApplyDelta reconstructs a file from a base file and delta ops.
func ApplyDelta(base []byte, ops DeltaOps, blockSize int) []byte {
	if blockSize <= 0 {
		blockSize = defaultBlockSize
	}

	out := make([]byte, 0, ops.TargetSize)
	for _, op := range ops.Ops {
		if op.IsCopy {
			start := int(op.CopyIndex) * blockSize
			end := start + blockSize
			if end > len(base) {
				end = len(base)
			}
			if start < len(base) {
				out = append(out, base[start:end]...)
			}
		} else {
			out = append(out, op.Data...)
		}
	}
	return out
}

// ToProto converts DeltaOps to a protobuf DeltaResponse.
func (d DeltaOps) ToProto(targetDigest []byte) *DeltaResponse {
	resp := &DeltaResponse{
		TargetDigest: targetDigest,
		TargetSize:   uint64(d.TargetSize),
	}
	for _, op := range d.Ops {
		if op.IsCopy {
			resp.Ops = append(resp.Ops, &DeltaOp{
				Op: &DeltaOp_CopyIndex{CopyIndex: op.CopyIndex},
			})
		} else {
			resp.Ops = append(resp.Ops, &DeltaOp{
				Op: &DeltaOp_Data{Data: op.Data},
			})
		}
	}
	return resp
}

// DeltaOpsFromProto converts a protobuf DeltaResponse to DeltaOps.
func DeltaOpsFromProto(resp *DeltaResponse) DeltaOps {
	d := DeltaOps{TargetSize: int64(resp.TargetSize)}
	for _, op := range resp.Ops {
		switch v := op.Op.(type) {
		case *DeltaOp_CopyIndex:
			d.Ops = append(d.Ops, deltaOp{IsCopy: true, CopyIndex: v.CopyIndex})
		case *DeltaOp_Data:
			d.Ops = append(d.Ops, deltaOp{Data: v.Data})
		}
	}
	return d
}

// weakHash computes the rsync rolling weak checksum for a block.
// Uses the classic two-component approach:
//
//	r1 = Σ(bytes) mod 65536
//	r2 = Σ((blocksize - i) * byte[i]) mod 65536
//	weak = r1 + 65536 * r2
func weakHash(data []byte) uint32 {
	var r1, r2 uint32
	n := len(data)
	for i, b := range data {
		r1 += uint32(b)
		r2 += uint32(n-i) * uint32(b)
	}
	r1 &= 0xFFFF
	r2 &= 0xFFFF
	return r1 | (r2 << 16)
}
