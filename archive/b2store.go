package archive

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Backblaze/blazer/b2"
)

// B2Config is what NewB2Storage needs.
type B2Config struct {
	KeyID        string
	AppKey       string
	Bucket       string
	BucketPrefix string // joined onto Keys during Upload/HeadByKey
}

// B2Storage is the blazer-backed ArchiveStorage used in production.
type B2Storage struct {
	bucket *b2.Bucket
	prefix string
}

// NewB2Storage authorizes against B2, resolves the bucket, and returns a
// ready-to-use storage. The returned cleanup func is currently a no-op
// (blazer maintains its own auth/upload-URL pools) but kept for wire
// consistency.
func NewB2Storage(ctx context.Context, cfg B2Config) (*B2Storage, func(), error) {
	if cfg.KeyID == "" || cfg.AppKey == "" {
		return nil, nil, fmt.Errorf("archive/b2: missing KeyID/AppKey")
	}
	if cfg.Bucket == "" {
		return nil, nil, fmt.Errorf("archive/b2: missing bucket")
	}
	client, err := b2.NewClient(ctx, cfg.KeyID, cfg.AppKey)
	if err != nil {
		return nil, nil, fmt.Errorf("archive/b2: auth: %w", err)
	}
	bucket, err := client.Bucket(ctx, cfg.Bucket)
	if err != nil {
		return nil, nil, fmt.Errorf("archive/b2: resolve bucket %q: %w", cfg.Bucket, err)
	}
	s := &B2Storage{bucket: bucket, prefix: cfg.BucketPrefix}
	return s, func() {}, nil
}

// KeyFor joins the configured bucket prefix with a hub-relative path.
// Exposed so the reconciler can form the same key when inserting into
// hub_entry.archive_file_id and when calling the storage.
func (s *B2Storage) KeyFor(relPath string) string {
	return s.prefix + relPath
}

func (s *B2Storage) Upload(ctx context.Context, req UploadRequest) (RemoteInfo, error) {
	obj := s.bucket.Object(req.Key)
	attrs := &b2.Attrs{
		ContentType: "application/octet-stream",
		Info: map[string]string{
			MetaHubsyncDigest:     hex.EncodeToString(req.Digest),
			MetaHubsyncDigestAlgo: req.DigestAlgo,
		},
	}
	if !req.MTime.IsZero() {
		attrs.Info[MetaSrcLastModifiedMS] = strconv.FormatInt(req.MTime.UnixMilli(), 10)
		attrs.LastModified = req.MTime
	}
	if req.ContentSHA1 != "" {
		attrs.SHA1 = req.ContentSHA1
	}

	w := obj.NewWriter(ctx, b2.WithAttrsOption(attrs))
	if _, err := io.Copy(w, req.Source); err != nil {
		// Best-effort cancel of an in-flight large upload so we don't leak
		// partial parts. Single-part uploads just fail without creating a
		// version.
		_ = w.Close()
		_ = obj.Cancel(ctx)
		return RemoteInfo{}, fmt.Errorf("archive/b2: upload stream: %w", err)
	}
	if err := w.Close(); err != nil {
		return RemoteInfo{}, fmt.Errorf("archive/b2: upload close: %w", err)
	}
	// After Close, obj is bound to the version we just uploaded.
	got, err := obj.Attrs(ctx)
	if err != nil {
		return RemoteInfo{}, fmt.Errorf("archive/b2: post-upload attrs: %w", err)
	}
	return toRemoteInfo(req.Key, obj.ID(), got), nil
}

func (s *B2Storage) HeadByKey(ctx context.Context, key string) (RemoteInfo, error) {
	obj := s.bucket.Object(key)
	got, err := obj.Attrs(ctx)
	if err != nil {
		if b2.IsNotExist(err) {
			return RemoteInfo{}, ErrNotExist
		}
		return RemoteInfo{}, fmt.Errorf("archive/b2: head %q: %w", key, err)
	}
	return toRemoteInfo(key, obj.ID(), got), nil
}

func (s *B2Storage) Download(ctx context.Context, key string, w io.Writer) error {
	obj := s.bucket.Object(key)
	r := obj.NewReader(ctx)
	defer r.Close()
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("archive/b2: download %q: %w", key, err)
	}
	return nil
}

func (s *B2Storage) PresignDownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	obj := s.bucket.Object(key)
	u, err := obj.AuthURL(ctx, ttl, "")
	if err != nil {
		return "", fmt.Errorf("archive/b2: presign %q: %w", key, err)
	}
	return u.String(), nil
}

// toRemoteInfo maps a blazer *Attrs into our RemoteInfo, decoding the hubsync
// fileInfo keys.
func toRemoteInfo(key, fileID string, a *b2.Attrs) RemoteInfo {
	out := RemoteInfo{
		FileID:      fileID,
		Key:         key,
		Size:        a.Size,
		ContentSHA1: a.SHA1,
		UploadedAt:  a.UploadTimestamp,
	}
	if a.Info != nil {
		if hexDigest, ok := a.Info[MetaHubsyncDigest]; ok {
			if b, err := hex.DecodeString(hexDigest); err == nil {
				out.Digest = b
			}
		}
		if algo, ok := a.Info[MetaHubsyncDigestAlgo]; ok {
			out.DigestAlgo = algo
		}
	}
	return out
}

// Compile-time interface check.
var _ ArchiveStorage = (*B2Storage)(nil)
