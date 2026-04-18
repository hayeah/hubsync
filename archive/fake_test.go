package archive

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFakeStorageUploadAndHead(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStorage()

	body := "hello world"
	info, err := s.Upload(ctx, UploadRequest{
		Key:        "backups/laptop/readme.txt",
		Size:       int64(len(body)),
		Source:     strings.NewReader(body),
		Digest:     []byte{0xde, 0xad, 0xbe, 0xef},
		DigestAlgo: "xxh128",
		MTime:      time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if info.FileID == "" {
		t.Fatal("expected fileId from upload")
	}

	head, err := s.HeadByKey(ctx, "backups/laptop/readme.txt")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.FileID != info.FileID {
		t.Errorf("head fileId=%q want %q", head.FileID, info.FileID)
	}
	if head.Size != int64(len(body)) {
		t.Errorf("head size=%d want %d", head.Size, len(body))
	}
	if head.DigestAlgo != "xxh128" {
		t.Errorf("head algo=%q", head.DigestAlgo)
	}
	if !bytes.Equal(head.Digest, info.Digest) {
		t.Error("head digest round-trip mismatch")
	}
}

func TestFakeStorageHeadMissing(t *testing.T) {
	_, err := NewFakeStorage().HeadByKey(context.Background(), "absent")
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestFakeStorageDownload(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStorage()
	_, _ = s.Upload(ctx, UploadRequest{Key: "a", Source: strings.NewReader("abc"), Size: 3})

	var buf bytes.Buffer
	if err := s.Download(ctx, "a", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "abc" {
		t.Errorf("download got %q", buf.String())
	}
}

func TestFakeStorageUploadCreatesNewVersion(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStorage()
	_, _ = s.Upload(ctx, UploadRequest{Key: "a", Source: strings.NewReader("v1"), Size: 2})
	_, _ = s.Upload(ctx, UploadRequest{Key: "a", Source: strings.NewReader("v2"), Size: 2})
	if got := s.VersionCount("a"); got != 2 {
		t.Errorf("version count = %d, want 2", got)
	}
	var buf bytes.Buffer
	_ = s.Download(ctx, "a", &buf)
	if buf.String() != "v2" {
		t.Errorf("download should return latest, got %q", buf.String())
	}
}

func TestFakeStoragePresignRequiresUploaded(t *testing.T) {
	s := NewFakeStorage()
	_, err := s.PresignDownloadURL(context.Background(), "missing", time.Minute)
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	_, _ = s.Upload(context.Background(), UploadRequest{Key: "p", Source: strings.NewReader("x"), Size: 1})
	got, err := s.PresignDownloadURL(context.Background(), "p", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "p") {
		t.Errorf("presign url = %q", got)
	}
}

func TestFakeStorageListKeysFlat(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStorage()
	for _, k := range []string{"a/1", "a/2", "b/1", "a/sub/3"} {
		_, _ = s.Upload(ctx, UploadRequest{Key: k, Source: strings.NewReader("x"), Size: 1})
	}

	it := s.ListKeys(ctx, "a/", "")
	var got []string
	for it.Next() {
		got = append(got, it.Entry().Key)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{"a/1", "a/2", "a/sub/3"}
	if !stringsEqual(got, want) {
		t.Errorf("flat list = %v, want %v", got, want)
	}
}

func TestFakeStorageListKeysDelimiter(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStorage()
	for _, k := range []string{"p/sid1/a", "p/sid1/b", "p/sid2/c", "p/top"} {
		_, _ = s.Upload(ctx, UploadRequest{Key: k, Source: strings.NewReader("x"), Size: 1})
	}

	it := s.ListKeys(ctx, "p/", "/")
	var got []string
	for it.Next() {
		got = append(got, it.Entry().Key)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{"p/sid1/", "p/sid2/", "p/top"}
	if !stringsEqual(got, want) {
		t.Errorf("delim list = %v, want %v", got, want)
	}
}

func TestFakeStorageDeleteByKey(t *testing.T) {
	ctx := context.Background()
	s := NewFakeStorage()
	info, _ := s.Upload(ctx, UploadRequest{Key: "k", Source: strings.NewReader("x"), Size: 1})

	// wrong fileID → ErrFileIDMismatch, no deletion
	if err := s.DeleteByKey(ctx, "k", "wrong-id"); !errors.Is(err, ErrFileIDMismatch) {
		t.Fatalf("want ErrFileIDMismatch, got %v", err)
	}
	if s.VersionCount("k") != 1 {
		t.Fatal("mismatch delete must not remove versions")
	}

	// correct fileID → success
	if err := s.DeleteByKey(ctx, "k", info.FileID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if s.VersionCount("k") != 0 {
		t.Fatal("delete should remove all versions")
	}

	// not-exist → success (idempotent)
	if err := s.DeleteByKey(ctx, "k", ""); err != nil {
		t.Fatalf("delete-missing must be nil, got %v", err)
	}
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
