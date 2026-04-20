package hubsync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"

	"github.com/hayeah/hubsync/archive"
	"github.com/hayeah/hubsync/internal/taskrunner"
)

// ArchiveTask is one unit of work for the taskrunner-backed archive flow:
// upload one hub_entry row to B2 and flip it to 'archived'. Fields with
// JSON tags are serialized into the SQLite items table; the transient
// fields tagged `json:"-"` are populated at Hydrate time and carry the
// dependencies Run needs.
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

	// transient deps — set by Hydrate, not serialized.
	store   *HubStore              `json:"-"`
	storage archive.ArchiveStorage `json:"-"`
	hasher  Hasher                 `json:"-"`
	hubDir  string                 `json:"-"`
	prefix  string                 `json:"-"`
}

// ArchiveTaskDeps groups the wiring Hydrate needs. Constructed once by
// the caller (cli/hubsync/archive.go) and closed over by the factory.
type ArchiveTaskDeps struct {
	Store   *HubStore
	Storage archive.ArchiveStorage
	Hasher  Hasher
	HubDir  string
	Prefix  string
}

// ArchiveTaskFactory implements taskrunner.TaskFactory for the archive
// flow. List enumerates pending hub_entry rows as ArchiveTasks; Hydrate
// returns a bare ArchiveTask with deps wired, into which the runner
// unmarshals the persisted row fields.
type ArchiveTaskFactory struct {
	deps ArchiveTaskDeps
}

// NewArchiveTaskFactory constructs a factory over the given deps.
func NewArchiveTaskFactory(deps ArchiveTaskDeps) *ArchiveTaskFactory {
	return &ArchiveTaskFactory{deps: deps}
}

// List emits one ArchiveTask per pending hub_entry row. Honors ctx so
// a Ctrl-C during baseline enumeration on a large hub terminates
// promptly.
func (f *ArchiveTaskFactory) List(ctx context.Context) iter.Seq2[taskrunner.Task, error] {
	return func(yield func(taskrunner.Task, error) bool) {
		rows, err := f.deps.Store.PendingArchiveRows()
		if err != nil {
			yield(nil, fmt.Errorf("plan archive tasks: %w", err))
			return
		}
		for _, r := range rows {
			if ctx.Err() != nil {
				yield(nil, ctx.Err())
				return
			}
			t := &ArchiveTask{
				ID:        r.Path,
				Path:      r.Path,
				Size:      r.Size,
				DigestHex: hex.EncodeToString(r.Digest.Bytes()),
				MTime:     r.MTime,
			}
			if !yield(t, nil) {
				return
			}
		}
	}
}

// Hydrate returns a fresh ArchiveTask with deps pre-wired. The runner
// json.Unmarshal's the items row into it after this call; the `json:"-"`
// dep fields survive the unmarshal.
func (f *ArchiveTaskFactory) Hydrate(ctx context.Context) (taskrunner.Task, error) {
	return &ArchiveTask{
		store:   f.deps.Store,
		storage: f.deps.Storage,
		hasher:  f.deps.Hasher,
		hubDir:  f.deps.HubDir,
		prefix:  f.deps.Prefix,
	}, nil
}

// Run uploads the file at Path to B2 and marks hub_entry as archived.
// Delegates to archiveOne — see that helper for the idempotency
// contract shared with the watch-mode worker.
func (t *ArchiveTask) Run(ctx context.Context) (any, error) {
	if t.store == nil || t.storage == nil || t.hasher == nil {
		return nil, errors.New("ArchiveTask: missing deps (Hydrate not wired)")
	}
	return archiveOne(ctx, ArchiveTaskDeps{
		Store:   t.store,
		Storage: t.storage,
		Hasher:  t.hasher,
		HubDir:  t.hubDir,
		Prefix:  t.prefix,
	}, t.Path)
}
