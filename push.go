package hubsync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ProcessPush handles a PushRequest on the hub side.
// Each op is checked for version conflicts. No content-level merging —
// files are opaque blobs. On conflict, the current version/digest is returned
// so the client can pull and retry.
func (h *Hub) ProcessPush(req *PushRequest) *PushResponse {
	resp := &PushResponse{}
	for _, op := range req.Ops {
		resp.Results = append(resp.Results, h.processPushOp(op, req.OnConflict))
	}
	return resp
}

func (h *Hub) processPushOp(op *PushOp, policy ConflictPolicy) *PushResult {
	switch op.Op {
	case OpKind_OP_CREATE:
		return h.pushCreate(op, policy)
	case OpKind_OP_UPDATE:
		return h.pushUpdate(op, policy)
	case OpKind_OP_DELETE:
		return h.pushDelete(op, policy)
	default:
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason: fmt.Sprintf("unknown op kind: %d", op.Op),
			}},
		}
	}
}

func (h *Hub) pushCreate(op *PushOp, policy ConflictPolicy) *PushResult {
	existing, exists := h.Store.TreeLookup(op.Path)
	if !exists {
		return h.acceptWrite(op, OpCreate)
	}

	// File already exists
	if policy == ConflictPolicy_CLIENT_WINS {
		return h.acceptWrite(op, OpUpdate)
	}

	return &PushResult{
		Path: op.Path,
		Outcome: &PushResult_Conflict{Conflict: &PushConflict{
			Reason:         "file already exists",
			CurrentVersion: uint64(h.versionForEntry(existing)),
			CurrentDigest:  existing.Digest.Bytes(),
		}},
	}
}

func (h *Hub) pushUpdate(op *PushOp, policy ConflictPolicy) *PushResult {
	existing, exists := h.Store.TreeLookup(op.Path)
	if !exists {
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason: "file not found on hub",
			}},
		}
	}

	currentVersion := h.versionForEntry(existing)

	// Version matches — accept directly
	if uint64(currentVersion) == op.BaseVersion {
		return h.acceptWrite(op, OpUpdate)
	}

	// Stale version — conflict
	if policy == ConflictPolicy_CLIENT_WINS {
		return h.acceptWrite(op, OpUpdate)
	}

	if policy == ConflictPolicy_HUB_WINS {
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason:         "version conflict (hub wins — change dropped)",
				CurrentVersion: uint64(currentVersion),
				CurrentDigest:  existing.Digest.Bytes(),
			}},
		}
	}

	// FAIL policy
	return &PushResult{
		Path: op.Path,
		Outcome: &PushResult_Conflict{Conflict: &PushConflict{
			Reason:         "version conflict",
			CurrentVersion: uint64(currentVersion),
			CurrentDigest:  existing.Digest.Bytes(),
		}},
	}
}

func (h *Hub) pushDelete(op *PushOp, policy ConflictPolicy) *PushResult {
	existing, exists := h.Store.TreeLookup(op.Path)
	if !exists {
		// Already deleted — no-op
		return &PushResult{
			Path:    op.Path,
			Outcome: &PushResult_Accepted{Accepted: &PushAccepted{}},
		}
	}

	currentVersion := h.versionForEntry(existing)

	if uint64(currentVersion) != op.BaseVersion && policy == ConflictPolicy_FAIL {
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason:         "version conflict on delete",
				CurrentVersion: uint64(currentVersion),
				CurrentDigest:  existing.Digest.Bytes(),
			}},
		}
	}

	fullPath := filepath.Join(h.Scanner.dir, op.Path)
	os.Remove(fullPath)

	entry := ChangeEntry{
		Path:  op.Path,
		Op:    OpDelete,
		Kind:  FileKindFile,
		MTime: time.Now().Unix(),
	}

	version, err := h.Store.Append(entry)
	if err != nil {
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason: fmt.Sprintf("store error: %v", err),
			}},
		}
	}

	entry.Version = version
	h.Broadcaster.Broadcast(entry)

	return &PushResult{
		Path:    op.Path,
		Outcome: &PushResult_Accepted{Accepted: &PushAccepted{Version: uint64(version)}},
	}
}

// acceptWrite writes data to disk and appends to the store.
func (h *Hub) acceptWrite(op *PushOp, changeOp ChangeOp) *PushResult {
	fullPath := filepath.Join(h.Scanner.dir, op.Path)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason: fmt.Sprintf("mkdir: %v", err),
			}},
		}
	}

	if err := os.WriteFile(fullPath, op.Data, 0644); err != nil {
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason: fmt.Sprintf("write: %v", err),
			}},
		}
	}

	digest := h.Hasher.Sum(op.Data)
	now := time.Now().Unix()

	entry := ChangeEntry{
		Path:   op.Path,
		Op:     changeOp,
		Kind:   FileKindFile,
		Digest: digest,
		Size:   int64(len(op.Data)),
		Mode:   0644,
		MTime:  now,
	}

	version, err := h.Store.Append(entry)
	if err != nil {
		return &PushResult{
			Path: op.Path,
			Outcome: &PushResult_Conflict{Conflict: &PushConflict{
				Reason: fmt.Sprintf("store error: %v", err),
			}},
		}
	}

	entry.Version = version
	h.Broadcaster.Broadcast(entry)

	return &PushResult{
		Path:    op.Path,
		Outcome: &PushResult_Accepted{Accepted: &PushAccepted{Version: uint64(version)}},
	}
}

// versionForEntry looks up the version for a tree entry by finding it in the change log.
func (h *Hub) versionForEntry(entry TreeEntry) int64 {
	var version int64
	err := h.Store.DB.Get(&version,
		`SELECT MAX(version) FROM change_log WHERE path = ? AND op != 'delete'`,
		entry.Path,
	)
	if err == sql.ErrNoRows || err != nil {
		return 0
	}
	return version
}
