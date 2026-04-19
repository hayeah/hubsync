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

// archiveOne uploads one hub_entry path to remote storage and stamps the
// row to 'archived'. It is the shared core of the one-shot taskrunner
// path (ArchiveTask.Run) and the long-running watch path
// (ArchiveWorker.uploadOne) — both must share the same idempotency
// semantics so retries cannot stack B2 versions.
//
// Idempotency contract:
//   - Row missing / not a file / already archived / unpinned → no-op.
//   - Remote head exists and its stamped digest matches the local
//     digest → skip the upload, just MarkArchived from the head info.
//     This closes the crash-after-Upload-before-MarkArchived window.
//   - Otherwise upload + MarkArchived.
//
// The return value carries skip-reason metadata for taskrunner
// diagnostics. Callers that only care about success/failure (the
// worker) can discard it.
func archiveOne(ctx context.Context, deps ArchiveTaskDeps, path string) (any, error) {
	entry, ok, err := deps.Store.EntryLookup(path)
	if err != nil {
		return nil, fmt.Errorf("entry lookup %s: %w", path, err)
	}
	if !ok || entry.Kind != FileKindFile {
		return map[string]any{"skipped": "missing-or-not-file"}, nil
	}
	switch entry.ArchiveState {
	case ArchiveStateArchived, ArchiveStateUnpinned:
		return map[string]any{"skipped": string(entry.ArchiveState)}, nil
	}

	key := deps.Prefix + path

	if head, err := deps.Storage.HeadByKey(ctx, key); err == nil {
		if headMatches(head, entry.Digest.Bytes(), deps.Hasher.Name()) {
			var sha1Bytes []byte
			if head.ContentSHA1 != "" {
				if b, err := hex.DecodeString(head.ContentSHA1); err == nil {
					sha1Bytes = b
				}
			}
			if err := deps.Store.MarkArchived(path, head.FileID, sha1Bytes, head.UploadedAt.UnixMilli()); err != nil {
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

	fullPath := filepath.Join(deps.HubDir, path)
	src, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", fullPath, err)
	}
	defer src.Close()

	info, err := deps.Storage.Upload(ctx, archive.UploadRequest{
		Key:        key,
		Size:       entry.Size,
		Source:     src,
		Digest:     entry.Digest.Bytes(),
		DigestAlgo: deps.Hasher.Name(),
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
	if err := deps.Store.MarkArchived(path, info.FileID, sha1Bytes, info.UploadedAt.UnixMilli()); err != nil {
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
