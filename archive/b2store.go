package archive

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
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

// ListKeys wraps blazer's bucket.List iterator. When delimiter is non-empty,
// B2 collapses keys at that separator and emits common-prefix markers whose
// Name ends in the delimiter; we surface those with RemoteInfo.Key set and
// everything else zero.
//
// Page size: blazer's ListPageSize is hard-capped at 1000 per call
// (iterator.go:94) even though B2's raw API supports up to 10000. We pass
// the max blazer allows; realistic prefixes fit in O(tens) of list calls,
// well inside B2's Class C free tier. Revisit only if a specific prefix
// ever makes listing cost material.
func (s *B2Storage) ListKeys(ctx context.Context, prefix, delimiter string) ListIterator {
	opts := []b2.ListOption{
		b2.ListPrefix(prefix),
		b2.ListPageSize(1000),
	}
	if delimiter != "" {
		opts = append(opts, b2.ListDelimiter(delimiter))
	}
	return &b2Iterator{
		ctx:       ctx,
		inner:     s.bucket.List(ctx, opts...),
		delimiter: delimiter,
	}
}

// DeleteByKey removes the head version at key via blazer's Object.Delete.
// Object.Delete internally calls ensure(), which HEADs the key to resolve
// the live FileID; we piggy-back that HEAD and compare against wantFileID
// before issuing the delete, so the guard adds zero extra API calls.
func (s *B2Storage) DeleteByKey(ctx context.Context, key, wantFileID string) error {
	obj := s.bucket.Object(key)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if b2.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("archive/b2: delete-head %q: %w", key, err)
	}
	_ = attrs // blazer populates obj.ID() from the ensure/Attrs pair
	if wantFileID != "" && obj.ID() != wantFileID {
		return fmt.Errorf("%w: key=%s want=%s got=%s", ErrFileIDMismatch, key, wantFileID, obj.ID())
	}
	if err := obj.Delete(ctx); err != nil {
		if b2.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("archive/b2: delete %q: %w", key, err)
	}
	return nil
}

type b2Iterator struct {
	ctx       context.Context
	inner     *b2.ObjectIterator
	delimiter string
	cur       RemoteInfo
	primed    bool
}

func (it *b2Iterator) Next() bool {
	if !it.inner.Next() {
		return false
	}
	obj := it.inner.Object()
	name := obj.Name()
	// Common-prefix markers: blazer returns a pseudo-object whose name ends
	// in the delimiter. Their Attrs are synthetic; don't call Attrs on them.
	if it.delimiter != "" && strings.HasSuffix(name, it.delimiter) {
		it.cur = RemoteInfo{Key: name}
		it.primed = true
		return true
	}
	attrs, err := obj.Attrs(it.ctx)
	if err != nil {
		// Surface via Err().
		it.inner = nil
		return false
	}
	it.cur = toRemoteInfo(name, obj.ID(), attrs)
	it.primed = true
	return true
}

func (it *b2Iterator) Entry() RemoteInfo {
	if !it.primed {
		return RemoteInfo{}
	}
	return it.cur
}

func (it *b2Iterator) Err() error {
	if it.inner == nil {
		return fmt.Errorf("archive/b2: list iteration failed")
	}
	return it.inner.Err()
}

// Compile-time interface check.
var _ ArchiveStorage = (*B2Storage)(nil)
