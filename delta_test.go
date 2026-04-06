package hubsync

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestOptimalBlockSize(t *testing.T) {
	tests := []struct {
		fileSize int64
		wantMin  int
		wantMax  int
	}{
		{0, defaultBlockSize, defaultBlockSize},
		{100, minBlockSize, minBlockSize},           // sqrt(2400) = 48, clamped to 1KB
		{1024 * 1024, minBlockSize, maxBlockSize},    // sqrt(24M) ≈ 4899
		{100 * 1024 * 1024, minBlockSize, maxBlockSize}, // sqrt(2.4G) ≈ 48990
	}
	for _, tt := range tests {
		bs := OptimalBlockSize(tt.fileSize)
		if bs < tt.wantMin || bs > tt.wantMax {
			t.Errorf("OptimalBlockSize(%d) = %d, want [%d, %d]", tt.fileSize, bs, tt.wantMin, tt.wantMax)
		}
	}
}

func TestWeakHashDeterministic(t *testing.T) {
	data := []byte("hello world, this is a test block")
	h1 := weakHash(data)
	h2 := weakHash(data)
	if h1 != h2 {
		t.Errorf("weak hash not deterministic: %d != %d", h1, h2)
	}
}

func TestWeakHashDifferent(t *testing.T) {
	h1 := weakHash([]byte("block one data here"))
	h2 := weakHash([]byte("block two data here"))
	if h1 == h2 {
		t.Error("weak hash collision on different data (unlikely)")
	}
}

func TestComputeSignature(t *testing.T) {
	data := make([]byte, 3*1024) // 3KB
	for i := range data {
		data[i] = byte(i % 256)
	}

	sigs := ComputeSignature(data, 1024)
	if len(sigs) != 3 {
		t.Fatalf("expected 3 signatures, got %d", len(sigs))
	}

	for i, sig := range sigs {
		if sig.Index != uint32(i) {
			t.Errorf("sig %d index: got %d, want %d", i, sig.Index, i)
		}
		if sig.WeakHash == 0 {
			t.Errorf("sig %d has zero weak hash", i)
		}
	}
}

func TestComputeSignaturePartialLastBlock(t *testing.T) {
	data := make([]byte, 2500) // 2.5 * 1024, partial last block
	sigs := ComputeSignature(data, 1024)
	if len(sigs) != 3 {
		t.Fatalf("expected 3 signatures for 2500 bytes with 1024 block size, got %d", len(sigs))
	}
}

func TestDeltaIdenticalFiles(t *testing.T) {
	data := makeTestData(10 * 1024) // 10KB
	blockSize := 1024

	sigs := ComputeSignature(data, blockSize)
	delta := ComputeDelta(data, sigs, blockSize)

	// All ops should be copy ops
	for i, op := range delta.Ops {
		if !op.IsCopy {
			t.Errorf("op %d: expected copy, got data (%d bytes)", i, len(op.Data))
		}
	}

	// Reconstruct
	result := ApplyDelta(data, delta, blockSize)
	if !bytes.Equal(result, data) {
		t.Error("reconstructed file doesn't match original")
	}
}

func TestDeltaSmallEdit(t *testing.T) {
	base := makeTestData(10 * 1024) // 10KB
	target := make([]byte, len(base))
	copy(target, base)
	// Modify a few bytes in the middle
	target[5000] = 0xFF
	target[5001] = 0xFE
	target[5002] = 0xFD

	blockSize := 1024
	sigs := ComputeSignature(base, blockSize)
	delta := ComputeDelta(target, sigs, blockSize)

	// Should have mostly copy ops (9 out of 10 blocks unchanged)
	copyCount := 0
	dataBytes := 0
	for _, op := range delta.Ops {
		if op.IsCopy {
			copyCount++
		} else {
			dataBytes += len(op.Data)
		}
	}
	if copyCount < 9 {
		t.Errorf("expected at least 9 copy ops, got %d", copyCount)
	}
	// Data bytes should be much less than full file size
	if dataBytes >= len(target) {
		t.Errorf("delta data (%d bytes) should be less than full file (%d bytes)", dataBytes, len(target))
	}

	result := ApplyDelta(base, delta, blockSize)
	if !bytes.Equal(result, target) {
		t.Error("reconstructed file doesn't match target")
	}
}

func TestDeltaAppendedData(t *testing.T) {
	base := makeTestData(8 * 1024) // 8KB
	target := make([]byte, len(base)+2048)
	copy(target, base)
	// Append 2KB of new data
	for i := len(base); i < len(target); i++ {
		target[i] = byte(i % 251)
	}

	blockSize := 1024
	sigs := ComputeSignature(base, blockSize)
	delta := ComputeDelta(target, sigs, blockSize)

	result := ApplyDelta(base, delta, blockSize)
	if !bytes.Equal(result, target) {
		t.Error("reconstructed file doesn't match target after append")
	}
}

func TestDeltaPrependedData(t *testing.T) {
	base := makeTestData(8 * 1024)
	// Prepend 512 bytes — shifts all blocks
	prepend := make([]byte, 512)
	for i := range prepend {
		prepend[i] = byte(i % 199)
	}
	target := append(prepend, base...)

	blockSize := 1024
	sigs := ComputeSignature(base, blockSize)
	delta := ComputeDelta(target, sigs, blockSize)

	result := ApplyDelta(base, delta, blockSize)
	if !bytes.Equal(result, target) {
		t.Error("reconstructed file doesn't match target after prepend")
	}
}

func TestDeltaCompletelyDifferent(t *testing.T) {
	base := makeTestData(4 * 1024)
	target := make([]byte, 4*1024)
	rand.Read(target) // completely random

	blockSize := 1024
	sigs := ComputeSignature(base, blockSize)
	delta := ComputeDelta(target, sigs, blockSize)

	// Should be all data ops (no blocks match)
	for _, op := range delta.Ops {
		if op.IsCopy {
			t.Error("unexpected copy op for completely different files")
		}
	}

	result := ApplyDelta(base, delta, blockSize)
	if !bytes.Equal(result, target) {
		t.Error("reconstructed file doesn't match target")
	}
}

func TestDeltaEmptyTarget(t *testing.T) {
	base := makeTestData(4 * 1024)
	var target []byte

	blockSize := 1024
	sigs := ComputeSignature(base, blockSize)
	delta := ComputeDelta(target, sigs, blockSize)

	result := ApplyDelta(base, delta, blockSize)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d bytes", len(result))
	}
}

func TestDeltaEmptyBase(t *testing.T) {
	var base []byte
	target := makeTestData(4 * 1024)

	blockSize := 1024
	sigs := ComputeSignature(base, blockSize)
	delta := ComputeDelta(target, sigs, blockSize)

	result := ApplyDelta(base, delta, blockSize)
	if !bytes.Equal(result, target) {
		t.Error("reconstructed file doesn't match target with empty base")
	}
}

func TestDeltaProtoRoundTrip(t *testing.T) {
	base := makeTestData(8 * 1024)
	target := make([]byte, len(base))
	copy(target, base)
	target[4096] = 0xAA

	blockSize := 1024
	sigs := ComputeSignature(base, blockSize)
	delta := ComputeDelta(target, sigs, blockSize)

	// Convert to proto and back
	digest := ComputeDigest(target)
	protoResp := delta.ToProto(digest[:])
	restored := DeltaOpsFromProto(protoResp)

	result := ApplyDelta(base, restored, blockSize)
	if !bytes.Equal(result, target) {
		t.Error("roundtrip through proto failed")
	}
}

func makeTestData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	return data
}
