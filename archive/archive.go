// Package archive implements the B2-backed backup layer described in
// hubsync's archive spec. ArchiveStorage is the seam between the hub-side
// reconciler and the underlying object store; b2store.go is the production
// implementation backed by github.com/Backblaze/blazer.
package archive

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotExist is returned by HeadByKey / PresignDownloadURL when no visible
// version exists at the given key. Wraps errors.Is checkable.
var ErrNotExist = errors.New("archive: remote object does not exist")

// RemoteInfo summarizes the remote view of one archived file. It is populated
// from blazer's Attrs() / Bucket.List() responses and carried through the
// reconciler so state transitions only commit when we see the value we
// expect.
type RemoteInfo struct {
	FileID      string    // blazer's fileId — durable remote handle
	Key         string    // bucket-relative name (bucket_prefix + hub-relative path)
	Size        int64     // contentLength as B2 reports it
	Digest      []byte    // X-Bz-Info-hubsync_digest (raw bytes)
	DigestAlgo  string    // X-Bz-Info-hubsync_digest_algo (xxh128 | sha256)
	ContentSHA1 string    // B2 contentSha1 (hex) — transfer integrity
	UploadedAt  time.Time // server-side upload timestamp
}

// UploadRequest carries what we need to stream one file up to B2 and to stamp
// hubsync-specific fileInfo onto the resulting version.
type UploadRequest struct {
	Key        string // bucket-relative path (bucket_prefix + hub-relative path)
	Size       int64
	MTime      time.Time
	Source     io.Reader
	Digest     []byte // raw content hash bytes to stamp as hubsync_digest
	DigestAlgo string // xxh128 | sha256
	// ContentSHA1 is B2's transfer-integrity checksum. Pre-computed in the
	// same streaming pass as Digest; stored on B2 but never in hub_entry.
	// Leave empty to skip SHA-1 verification (not recommended).
	ContentSHA1 string
}

// ArchiveStorage abstracts the remote object store. Real impl uses blazer.
// Test doubles stub out Upload / Download / etc. The interface is
// key-addressed (not fileID-addressed) because blazer's public API works
// that way; the reconciler guards transitions by comparing the returned
// RemoteInfo.FileID against the hub_entry.archive_file_id it committed.
type ArchiveStorage interface {
	// Upload streams Source to the remote and returns the resulting
	// RemoteInfo (including the assigned fileId). On ctx cancellation, no
	// version is created (blazer's large-file path calls b2_cancel_large_file
	// internally).
	Upload(ctx context.Context, req UploadRequest) (RemoteInfo, error)

	// HeadByKey returns the current head version at key. Used during unpin
	// re-verification: callers compare RemoteInfo.FileID to their recorded
	// archive_file_id and abort the transition on mismatch.
	// Returns ErrNotExist if no visible version exists.
	HeadByKey(ctx context.Context, key string) (RemoteInfo, error)

	// Download streams the head version of key into w. The caller is
	// responsible for verifying the received bytes against hub_entry.digest
	// before flipping state to archived.
	Download(ctx context.Context, key string, w io.Writer) error

	// PresignDownloadURL returns a short-lived URL that can fetch key without
	// the hub's credentials. Used by /blobs/{digest} → 302 redirect when
	// the local file is absent and the row is unpinned.
	PresignDownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Metadata keys used on B2 fileInfo (X-Bz-Info-*).
const (
	MetaHubsyncDigest     = "hubsync_digest"
	MetaHubsyncDigestAlgo = "hubsync_digest_algo"
	MetaSrcLastModifiedMS = "src_last_modified_millis"
)
