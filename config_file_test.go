package hubsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigFile_EnvExpansion(t *testing.T) {
	t.Setenv("HUBSYNC_BUCKET", "my-bucket")
	t.Setenv("HUBSYNC_BUCKET_PREFIX", "my-prefix/")
	t.Setenv("B2_APPLICATION_KEY_ID", "key-id-42")
	t.Setenv("B2_APPLICATION_KEY", "app-key-shh")

	hubDir := t.TempDir()
	writeConfig(t, hubDir, `
[hub]
hash = "xxh128"

[archive]
provider      = "b2"
bucket        = "${HUBSYNC_BUCKET}"
bucket_prefix = "${HUBSYNC_BUCKET_PREFIX}"
b2_key_id     = "${B2_APPLICATION_KEY_ID}"
b2_app_key    = "${B2_APPLICATION_KEY}"
`)

	cf, err := LoadConfigFile(hubDir)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cf.Archive == nil {
		t.Fatal("expected [archive] section")
	}
	cases := []struct {
		got, want, name string
	}{
		{cf.Archive.Bucket, "my-bucket", "bucket"},
		{cf.Archive.BucketPrefix, "my-prefix/", "bucket_prefix"},
		{cf.Archive.B2KeyID, "key-id-42", "b2_key_id"},
		{cf.Archive.B2AppKey, "app-key-shh", "b2_app_key"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestLoadConfigFile_UnsetVarsExpandToEmpty(t *testing.T) {
	// Ensure none of the referenced vars are set.
	os.Unsetenv("HUBSYNC_BUCKET")
	os.Unsetenv("B2_APPLICATION_KEY_ID")
	os.Unsetenv("B2_APPLICATION_KEY")

	hubDir := t.TempDir()
	writeConfig(t, hubDir, `
[hub]
hash = "xxh128"

[archive]
provider   = "b2"
bucket     = "${HUBSYNC_BUCKET}"
b2_key_id  = "${B2_APPLICATION_KEY_ID}"
b2_app_key = "${B2_APPLICATION_KEY}"
`)

	cf, err := LoadConfigFile(hubDir)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cf.Archive.Bucket != "" {
		t.Errorf("bucket: got %q, want empty", cf.Archive.Bucket)
	}
	if cf.Archive.B2KeyID != "" {
		t.Errorf("b2_key_id: got %q, want empty", cf.Archive.B2KeyID)
	}
	if cf.Archive.B2AppKey != "" {
		t.Errorf("b2_app_key: got %q, want empty", cf.Archive.B2AppKey)
	}
}

func TestLoadConfigFile_BucketPrefixDefaultsToHubDirname(t *testing.T) {
	os.Unsetenv("HUBSYNC_BUCKET_PREFIX")

	parent := t.TempDir()
	hubDir := filepath.Join(parent, "laptop-home")
	if err := os.Mkdir(hubDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, hubDir, `
[hub]
hash = "xxh128"

[archive]
provider      = "b2"
bucket        = "my-bucket"
bucket_prefix = "${HUBSYNC_BUCKET_PREFIX}"
`)

	cf, err := LoadConfigFile(hubDir)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if got, want := cf.Archive.BucketPrefix, "laptop-home/"; got != want {
		t.Errorf("bucket_prefix: got %q, want %q", got, want)
	}
}

func TestLoadConfigFile_BucketPrefixExplicitOverridesDefault(t *testing.T) {
	os.Unsetenv("HUBSYNC_BUCKET_PREFIX")

	hubDir := t.TempDir()
	writeConfig(t, hubDir, `
[hub]
hash = "xxh128"

[archive]
provider      = "b2"
bucket        = "my-bucket"
bucket_prefix = "custom/"
`)

	cf, err := LoadConfigFile(hubDir)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if got, want := cf.Archive.BucketPrefix, "custom/"; got != want {
		t.Errorf("bucket_prefix: got %q, want %q", got, want)
	}
}

func TestLoadConfigFile_BucketPrefixAppendsTrailingSlash(t *testing.T) {
	os.Unsetenv("HUBSYNC_BUCKET_PREFIX")

	hubDir := t.TempDir()
	writeConfig(t, hubDir, `
[hub]
hash = "xxh128"

[archive]
provider      = "b2"
bucket        = "my-bucket"
bucket_prefix = "no-trailing-slash"
`)

	cf, err := LoadConfigFile(hubDir)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if got, want := cf.Archive.BucketPrefix, "no-trailing-slash/"; got != want {
		t.Errorf("bucket_prefix: got %q, want %q", got, want)
	}
}

func writeConfig(t *testing.T, hubDir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(hubDir, ".hubsync"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, ".hubsync", "config.toml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
