package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// integrationEnv reads the three env vars that gate the live-bucket tests.
// Tests skip (not fail) when any is missing, so `go test ./...` stays green
// for contributors without credentials.
type integrationEnv struct {
	keyID  string
	appKey string
	bucket string
}

func loadIntegrationEnv(t *testing.T) integrationEnv {
	t.Helper()
	env := integrationEnv{
		keyID:  os.Getenv("B2_APPLICATION_KEY_ID"),
		appKey: os.Getenv("B2_APPLICATION_KEY"),
		bucket: os.Getenv("HUBSYNC_TEST_BUCKET"),
	}
	if env.keyID == "" || env.appKey == "" || env.bucket == "" {
		t.Skip("B2_APPLICATION_KEY_ID / B2_APPLICATION_KEY / HUBSYNC_TEST_BUCKET not set; skipping live B2 test")
	}
	return env
}

// uniquePrefix returns a path prefix that no prior test run will collide
// with, e.g. hubsync-itest/20260417-143015-abcdef/ .
func uniquePrefix(t *testing.T) string {
	t.Helper()
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("hubsync-itest/%s-%s/", time.Now().UTC().Format("20060102-150405"), hex.EncodeToString(r[:]))
}

func TestB2StorageRoundTrip(t *testing.T) {
	e := loadIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, cleanup, err := NewB2Storage(ctx, B2Config{
		KeyID:        e.keyID,
		AppKey:       e.appKey,
		Bucket:       e.bucket,
		BucketPrefix: uniquePrefix(t),
	})
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	defer cleanup()

	body := []byte("integration round-trip — hubsync BLOB digest round-trip")
	digest := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE} // any bytes — just want to round-trip the fileInfo
	key := store.KeyFor("hello.txt")

	info, err := store.Upload(ctx, UploadRequest{
		Key:        key,
		Size:       int64(len(body)),
		Source:     bytes.NewReader(body),
		Digest:     digest,
		DigestAlgo: "xxh128",
		MTime:      time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if info.FileID == "" {
		t.Fatal("no fileId returned")
	}

	// Head round-trip: hubsync_digest + algo stamped correctly.
	head, err := store.HeadByKey(ctx, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.FileID != info.FileID {
		t.Errorf("head fileId=%q want %q", head.FileID, info.FileID)
	}
	if !bytes.Equal(head.Digest, digest) {
		t.Errorf("head digest=%x want %x", head.Digest, digest)
	}
	if head.DigestAlgo != "xxh128" {
		t.Errorf("head algo=%q", head.DigestAlgo)
	}
	if head.Size != int64(len(body)) {
		t.Errorf("head size=%d want %d", head.Size, len(body))
	}

	// Download streams exact bytes.
	var buf bytes.Buffer
	if err := store.Download(ctx, key, &buf); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Errorf("download bytes differ")
	}

	// Presigned URL resolves over plain HTTP.
	url, err := store.PresignDownloadURL(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if !strings.Contains(url, "Authorization=") {
		t.Errorf("presigned url looks wrong: %s", url)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET presigned: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("presigned GET status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("presigned GET body differs (n=%d)", len(got))
	}
}

func TestB2StorageHeadMissing(t *testing.T) {
	e := loadIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, cleanup, err := NewB2Storage(ctx, B2Config{
		KeyID:        e.keyID,
		AppKey:       e.appKey,
		Bucket:       e.bucket,
		BucketPrefix: uniquePrefix(t),
	})
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	defer cleanup()

	_, err = store.HeadByKey(ctx, store.KeyFor("does-not-exist"))
	if err == nil {
		t.Fatal("expected head on missing key to error")
	}
	// blazer may return either ErrNotExist or its own wrapped 404. Both OK.
	if err != nil {
		t.Logf("head on missing: %v", err)
	}
}
