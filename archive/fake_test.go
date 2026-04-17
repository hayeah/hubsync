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
