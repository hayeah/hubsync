package hubsync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hayeah/hubsync/archive"
)

// ArchiveTask is one unit of work for the taskrunner-backed archive flow:
// upload one hub_entry row to B2 and flip it to 'archived'. Fields with
// JSON tags are serialized into the DuckDB items table; the transient
// fields below that JSON tag with "-" are populated at Plan / Decode time
// and carry the dependencies Run needs.
//
// Idempotency contract: Run may be invoked multiple times against the
// same path (after a crash-reclaim or a --where-driven subset re-run).
// It first checks whether the remote head already matches the local
// digest; if so, it just re-writes the archived state and returns.
type ArchiveTask struct {
	ID        string `json:"id"` // = Path; hub_entry.path is unique
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	DigestHex string `json:"digest"`
	MTime     int64  `json:"mtime"` // unix seconds

	// transient deps — set by Decode, not serialized.
	store   *HubStore         `json:"-"`
	storage archive.ArchiveStorage `json:"-"`
	hasher  Hasher            `json:"-"`
	hubDir  string            `json:"-"`
	prefix  string            `json:"-"`
}

// ArchiveTaskDeps groups the wiring Decode needs. Constructed once by
// the caller (cli/hubsync/archive.go) and closed over in Plan + Decode.
type ArchiveTaskDeps struct {
	Store   *HubStore
	Storage archive.ArchiveStorage
	Hasher  Hasher
	HubDir  string
	Prefix  string
}

// PlanArchiveTasks returns a plan callback that emits one ArchiveTask
// per pending hub_entry row. Intended for taskrunner.Config.Plan.
//
// Note: the returned tasks carry their transient dependency pointers
// pre-populated; but the taskrunner re-invokes via Decode on resume
// invocations (items already populated, no Plan call). So Plan is the
// "first run" path and Decode is the "general" path.
func PlanArchiveTasks(deps ArchiveTaskDeps) func(ctx context.Context, emit func(*ArchiveTask)) error {
	return func(ctx context.Context, emit func(*ArchiveTask)) error {
		rows, err := deps.Store.PendingArchiveRows()
		if err != nil {
			return fmt.Errorf("plan archive tasks: %w", err)
		}
		for _, r := range rows {
			emit(&ArchiveTask{
				ID:        r.Path,
				Path:      r.Path,
				Size:      r.Size,
				DigestHex: hex.EncodeToString(r.Digest.Bytes()),
				MTime:     r.MTime,
				store:     deps.Store,
				storage:   deps.Storage,
				hasher:    deps.Hasher,
				hubDir:    deps.HubDir,
				prefix:    deps.Prefix,
			})
		}
		return nil
	}
}

// DecodeArchiveTask returns a Decode callback that rehydrates an
// ArchiveTask from one items row with the given deps closed over.
func DecodeArchiveTask(deps ArchiveTaskDeps) func(row map[string]any) (*ArchiveTask, error) {
	return func(row map[string]any) (*ArchiveTask, error) {
		id, _ := row["id"].(string)
		path, _ := row["path"].(string)
		digestHex, _ := row["digest"].(string)
		size := asInt64(row["size"])
		mtime := asInt64(row["mtime"])
		t := &ArchiveTask{
			ID:        id,
			Path:      path,
			Size:      size,
			DigestHex: digestHex,
			MTime:     mtime,
			store:     deps.Store,
			storage:   deps.Storage,
			hasher:    deps.Hasher,
			hubDir:    deps.HubDir,
			prefix:    deps.Prefix,
		}
		return t, nil
	}
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case uint64:
		return int64(x)
	case uint32:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

// Run uploads the file at Path to B2 and marks hub_entry as archived.
// Idempotent: if the row is already archived / unpinned, or if the
// remote head matches the local digest, Run fast-paths without
// re-uploading.
func (t *ArchiveTask) Run(ctx context.Context) (any, error) {
	if t.store == nil || t.storage == nil || t.hasher == nil {
		return nil, errors.New("ArchiveTask: missing deps (Decode not wired)")
	}

	// Re-fetch the current hub_entry row — state may have moved since
	// plan time (e.g. the user ran `unpin` between plan and run).
	entry, ok, err := t.store.EntryLookup(t.Path)
	if err != nil {
		return nil, fmt.Errorf("entry lookup %s: %w", t.Path, err)
	}
	if !ok || entry.Kind != FileKindFile {
		return map[string]any{"skipped": "missing-or-not-file"}, nil
	}
	switch entry.ArchiveState {
	case ArchiveStateArchived, ArchiveStateUnpinned:
		return map[string]any{"skipped": string(entry.ArchiveState)}, nil
	}

	key := t.prefix + t.Path

	// Idempotency check: if a head already exists at key AND its digest
	// matches what we have locally, treat it as already-uploaded and
	// just stamp the hub_entry state. Guards against B2 version
	// stacking on stale-reclaim after a previous successful upload that
	// crashed before MarkArchived.
	if head, err := t.storage.HeadByKey(ctx, key); err == nil {
		if headMatches(head, entry.Digest.Bytes(), t.hasher.Name()) {
			var sha1Bytes []byte
			if head.ContentSHA1 != "" {
				if b, err := hex.DecodeString(head.ContentSHA1); err == nil {
					sha1Bytes = b
				}
			}
			if err := t.store.MarkArchived(t.Path, head.FileID, sha1Bytes, head.UploadedAt.UnixMilli()); err != nil {
				return nil, fmt.Errorf("mark archived (head-match): %w", err)
			}
			return map[string]any{
				"skipped":  "head-matches",
				"file_id":  head.FileID,
				"size":     head.Size,
				"uploaded": head.UploadedAt.UnixMilli(),
			}, nil
		}
	} else if !errors.Is(err, archive.ErrNotExist) {
		return nil, fmt.Errorf("head check %s: %w", key, err)
	}

	fullPath := filepath.Join(t.hubDir, t.Path)
	src, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", fullPath, err)
	}
	defer src.Close()

	info, err := t.storage.Upload(ctx, archive.UploadRequest{
		Key:        key,
		Size:       entry.Size,
		Source:     src,
		Digest:     entry.Digest.Bytes(),
		DigestAlgo: t.hasher.Name(),
		MTime:      time.Unix(entry.MTime, 0),
	})
	if err != nil {
		return nil, fmt.Errorf("upload %s: %w", key, err)
	}

	var sha1Bytes []byte
	if info.ContentSHA1 != "" {
		if b, err := hex.DecodeString(info.ContentSHA1); err == nil {
			sha1Bytes = b
		}
	}
	if err := t.store.MarkArchived(t.Path, info.FileID, sha1Bytes, info.UploadedAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("mark archived: %w", err)
	}
	return map[string]any{
		"file_id":  info.FileID,
		"size":     info.Size,
		"uploaded": info.UploadedAt.UnixMilli(),
	}, nil
}

// headMatches reports whether the remote head's stamped hubsync_digest
// matches the local bytes + algo. The remote stores hex; we already
// have bytes.
func headMatches(head archive.RemoteInfo, localDigest []byte, localAlgo string) bool {
	if len(head.Digest) == 0 || len(localDigest) == 0 {
		return false
	}
	if head.DigestAlgo != "" && head.DigestAlgo != localAlgo {
		return false
	}
	if len(head.Digest) != len(localDigest) {
		return false
	}
	for i := range head.Digest {
		if head.Digest[i] != localDigest[i] {
			return false
		}
	}
	return true
}
