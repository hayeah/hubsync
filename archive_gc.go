package hubsync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hayeah/hubsync/archive"
)

// ArchiveGC lists a B2 prefix one delimiter-level at a time, classifies each
// returned entry against local hub_entry claims, and deletes orphans.
//
// The design is intentionally minimal: one invocation ↔ one delimited list
// call (plus one flat list per orphan dir we actually delete). Operators
// drill deeper by re-running with a deeper prefix; there is no --depth
// flag because the B2 list API is single-level and any multi-level
// emulation belongs in the shell, not the tool.
type ArchiveGC struct {
	Store   *HubStore
	Storage archive.ArchiveStorage
	// Prefix is the hub's configured bucket_prefix. Used to translate
	// listed bucket-relative keys into hub-relative paths for claim-check.
	Prefix string
}

// ArchiveGCAction labels the outcome for one listed entry.
type ArchiveGCAction string

const (
	ArchiveGCKeep           ArchiveGCAction = "keep"
	ArchiveGCDelete         ArchiveGCAction = "delete"
	ArchiveGCDeleteSubtree  ArchiveGCAction = "delete_subtree"
)

// ArchiveGCEntry is one classified row in the archive-gc output. Serialized
// as JSONL by the CLI, following the `ls` convention (one Go struct with
// JSON tags; the wire body is the struct, not a projection).
type ArchiveGCEntry struct {
	Kind       string          `json:"kind"`                  // "file" | "dir"
	Key        string          `json:"key"`                   // bucket-relative; "/"-terminated for dirs
	Path       string          `json:"path,omitempty"`        // hub-relative; empty if key is outside the hub's bucket_prefix
	Claimed    bool            `json:"claimed"`
	Action     ArchiveGCAction `json:"action"`
	FileID     string          `json:"file_id,omitempty"`     // files only
	Size       int64           `json:"size,omitempty"`        // files only
	UploadedAt int64           `json:"uploaded_at,omitempty"` // files only; unix millis

	// Populated by non-dry runs.
	Deleted  bool   `json:"deleted,omitempty"`
	DeletedN int    `json:"deleted_n,omitempty"` // for delete_subtree: how many underlying files were removed
	Error    string `json:"error,omitempty"`
}

// ArchiveGCSummary aggregates per-entry outcomes for the CLI stderr line.
type ArchiveGCSummary struct {
	OrphanFiles  int
	OrphanDirs   int
	KeptFiles    int
	KeptDirs     int
	DeletedFiles int
	DeletedBytes int64
	Errors       int
}

// Run lists prefix with delimiter="/", classifies each entry, and (unless
// dry is true) issues deletes for orphans. emit is called once per
// top-level listed entry with the final populated ArchiveGCEntry (after
// deletion attempts). It is not called for individual files inside an
// orphan subtree — those are summarized via ArchiveGCEntry.DeletedN.
func (g *ArchiveGC) Run(ctx context.Context, prefix string, dry bool, emit func(ArchiveGCEntry)) (ArchiveGCSummary, error) {
	if g.Store == nil || g.Storage == nil {
		return ArchiveGCSummary{}, fmt.Errorf("archive-gc: store and storage required")
	}
	claims, err := g.collectClaims()
	if err != nil {
		return ArchiveGCSummary{}, fmt.Errorf("archive-gc: load hub_entry: %w", err)
	}

	var summary ArchiveGCSummary
	it := g.Storage.ListKeys(ctx, prefix, "/")
	for it.Next() {
		ri := it.Entry()
		entry := g.classify(ri, claims)

		if !dry {
			g.act(ctx, &entry, &summary)
		}

		switch entry.Action {
		case ArchiveGCDelete:
			summary.OrphanFiles++
		case ArchiveGCDeleteSubtree:
			summary.OrphanDirs++
		case ArchiveGCKeep:
			if entry.Kind == "file" {
				summary.KeptFiles++
			} else {
				summary.KeptDirs++
			}
		}

		emit(entry)
	}
	if err := it.Err(); err != nil {
		return summary, fmt.Errorf("archive-gc: list %q: %w", prefix, err)
	}
	return summary, nil
}

// collectClaims builds a snapshot of {key → claimed} and the set of hub-
// relative paths used for prefix-claim queries on dir entries.
func (g *ArchiveGC) collectClaims() (*claimSet, error) {
	rows, err := g.Store.EntrySnapshot()
	if err != nil {
		return nil, err
	}
	cs := &claimSet{
		fileKeys: make(map[string]struct{}, len(rows)),
		paths:    make([]string, 0, len(rows)),
	}
	for _, r := range rows {
		if r.Kind != FileKindFile {
			continue
		}
		key := archive.JoinKey(g.Prefix, r.Path)
		cs.fileKeys[key] = struct{}{}
		cs.paths = append(cs.paths, r.Path)
	}
	return cs, nil
}

// classify maps one listed entry to an ArchiveGCEntry with Action populated.
func (g *ArchiveGC) classify(ri archive.RemoteInfo, claims *claimSet) ArchiveGCEntry {
	key := ri.Key
	isDir := strings.HasSuffix(key, "/") && ri.FileID == ""
	hubRel := ""
	if strings.HasPrefix(key, g.Prefix) {
		hubRel = key[len(g.Prefix):]
	}

	if isDir {
		out := ArchiveGCEntry{
			Kind: "dir",
			Key:  key,
			Path: hubRel,
		}
		if hubRel != "" && claims.hasDirClaim(hubRel) {
			out.Claimed = true
			out.Action = ArchiveGCKeep
		} else {
			out.Action = ArchiveGCDeleteSubtree
		}
		return out
	}

	out := ArchiveGCEntry{
		Kind:       "file",
		Key:        key,
		Path:       hubRel,
		FileID:     ri.FileID,
		Size:       ri.Size,
		UploadedAt: ri.UploadedAt.UnixMilli(),
	}
	if _, ok := claims.fileKeys[key]; ok {
		out.Claimed = true
		out.Action = ArchiveGCKeep
	} else {
		out.Action = ArchiveGCDelete
	}
	return out
}

// act executes the decision stored in entry.Action, updating entry with
// Deleted/DeletedN/Error and bumping the summary.
func (g *ArchiveGC) act(ctx context.Context, entry *ArchiveGCEntry, summary *ArchiveGCSummary) {
	switch entry.Action {
	case ArchiveGCDelete:
		if err := g.Storage.DeleteByKey(ctx, entry.Key, entry.FileID); err != nil {
			entry.Error = err.Error()
			summary.Errors++
			return
		}
		entry.Deleted = true
		summary.DeletedFiles++
		summary.DeletedBytes += entry.Size
	case ArchiveGCDeleteSubtree:
		n, bytes, err := g.deleteSubtree(ctx, entry.Key)
		entry.DeletedN = n
		summary.DeletedFiles += n
		summary.DeletedBytes += bytes
		if err != nil {
			entry.Error = err.Error()
			summary.Errors++
			return
		}
		entry.Deleted = true
	}
}

// deleteSubtree walks every object under prefix (no delimiter) and deletes
// each one with the fileID guard. Returns the count and total bytes of
// successfully deleted objects and the first error encountered, if any.
// Continues deleting past individual errors so a single stuck file doesn't
// abort the whole subtree.
func (g *ArchiveGC) deleteSubtree(ctx context.Context, prefix string) (int, int64, error) {
	var (
		count    int
		bytes    int64
		firstErr error
	)
	it := g.Storage.ListKeys(ctx, prefix, "")
	for it.Next() {
		ri := it.Entry()
		if ri.FileID == "" {
			// Defensive: shouldn't happen with empty delimiter, but skip any
			// pseudo-entries anyway.
			continue
		}
		if err := g.Storage.DeleteByKey(ctx, ri.Key, ri.FileID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		count++
		bytes += ri.Size
	}
	if err := it.Err(); err != nil {
		return count, bytes, err
	}
	if firstErr != nil {
		return count, bytes, firstErr
	}
	return count, bytes, nil
}

// claimSet carries a snapshot of hub_entry's file paths, indexed for both
// exact-key and dir-prefix queries.
type claimSet struct {
	fileKeys map[string]struct{} // exact bucket-relative key → claimed
	paths    []string            // hub-relative paths; used by hasDirClaim
}

// hasDirClaim reports whether any hub_entry path starts with hubRel, where
// hubRel ends in "/" (i.e. is a dir marker in hub-relative form).
func (c *claimSet) hasDirClaim(hubRel string) bool {
	for _, p := range c.paths {
		if strings.HasPrefix(p, hubRel) {
			return true
		}
	}
	return false
}

// ErrArchiveNotConfigured is returned when the hub lacks an [archive]
// config section. archive-gc needs the storage layer to do anything.
var ErrArchiveNotConfigured = errors.New("archive not configured in .hubsync/config.toml")
