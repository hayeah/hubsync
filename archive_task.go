package hubsync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

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
// Delegates to archiveOne — see that helper for the idempotency
// contract shared with the watch-mode worker.
func (t *ArchiveTask) Run(ctx context.Context) (any, error) {
	if t.store == nil || t.storage == nil || t.hasher == nil {
		return nil, errors.New("ArchiveTask: missing deps (Decode not wired)")
	}
	return archiveOne(ctx, ArchiveTaskDeps{
		Store:   t.store,
		Storage: t.storage,
		Hasher:  t.hasher,
		HubDir:  t.hubDir,
		Prefix:  t.prefix,
	}, t.Path)
}
