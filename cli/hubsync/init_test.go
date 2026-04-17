package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayeah/hubsync"
)

func TestRunInit_CreatesConfig(t *testing.T) {
	dir := t.TempDir()

	path, err := runInit(dir)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	want := filepath.Join(dir, ".hubsync", "config.toml")
	if path != want {
		t.Errorf("path: got %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, defaultConfigTemplate) {
		t.Error("written config does not match embedded template")
	}
	// The scaffolded config should parse cleanly through LoadConfigFile
	// with the expected env-var substitutions.
	t.Setenv("HUBSYNC_BUCKET", "scaffold-bucket")
	t.Setenv("B2_APPLICATION_KEY_ID", "scaffold-key-id")
	t.Setenv("B2_APPLICATION_KEY", "scaffold-app-key")
	os.Unsetenv("HUBSYNC_BUCKET_PREFIX")

	cf, err := hubsync.LoadConfigFile(dir)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cf.Archive == nil {
		t.Fatal("expected [archive] section after init")
	}
	if got, want := cf.Archive.Bucket, "scaffold-bucket"; got != want {
		t.Errorf("bucket: got %q, want %q", got, want)
	}
	// Hub dir basename + "/" when HUBSYNC_BUCKET_PREFIX is unset.
	wantPrefix := filepath.Base(dir) + "/"
	if cf.Archive.BucketPrefix != wantPrefix {
		t.Errorf("bucket_prefix: got %q, want %q", cf.Archive.BucketPrefix, wantPrefix)
	}
}

func TestRunInit_RefusesToClobber(t *testing.T) {
	dir := t.TempDir()

	if _, err := runInit(dir); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	_, err := runInit(dir)
	if err == nil {
		t.Fatal("second runInit: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error: got %q, want substring %q", err.Error(), "already exists")
	}
}
