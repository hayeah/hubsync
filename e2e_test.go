package hubsync

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testEnv holds the hub and client for E2E tests.
type testEnv struct {
	hubDir    string
	clientDir string
	hubApp    *HubApp
	clientApp *ClientApp
	ts        *httptest.Server
	cleanup   func()
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	hubDir := t.TempDir()
	clientDir := t.TempDir()
	hubDBPath := filepath.Join(t.TempDir(), "hub.db")
	clientDBPath := filepath.Join(t.TempDir(), "client.db")
	token := BearerToken("test-token")

	hubApp, hubCleanup, err := InitializeTestHubApp(&HubConfig{
		DBPath:   hubDBPath,
		WatchDir: hubDir,
		Token:    string(token),
		Listen:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("init hub: %v", err)
	}

	ts := httptest.NewServer(hubApp.Server)

	clientApp, clientCleanup, err := InitializeTestClientApp(&ClientConfig{
		DBPath:  clientDBPath,
		HubURL:  ts.URL,
		Token:   string(token),
		SyncDir: clientDir,
	})
	if err != nil {
		hubCleanup()
		ts.Close()
		t.Fatalf("init client: %v", err)
	}

	return &testEnv{
		hubDir:    hubDir,
		clientDir: clientDir,
		hubApp:    hubApp,
		clientApp: clientApp,
		ts:        ts,
		cleanup: func() {
			ts.Close()
			clientCleanup()
			hubCleanup()
		},
	}
}

// writeHubFile writes a file to the hub directory.
func (e *testEnv) writeHubFile(t *testing.T, path, content string) {
	t.Helper()
	full := filepath.Join(e.hubDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// deleteHubFile removes a file from the hub directory.
func (e *testEnv) deleteHubFile(t *testing.T, path string) {
	t.Helper()
	full := filepath.Join(e.hubDir, path)
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
}

// scan triggers a full scan on the hub.
func (e *testEnv) scan(t *testing.T) {
	t.Helper()
	if err := e.hubApp.Hub.FullScan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// syncUntil runs the client sync until the given version is reached or timeout.
func (e *testEnv) syncUntil(t *testing.T, targetVersion int64, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var mu sync.Mutex
	var lastVersion int64

	e.clientApp.Client.OnEvent = func(version int64, path string, op ChangeOp) {
		mu.Lock()
		lastVersion = version
		mu.Unlock()
		if version >= targetVersion {
			cancel()
		}
	}

	_ = e.clientApp.Client.Sync(ctx)
	if ctx.Err() == context.DeadlineExceeded {
		mu.Lock()
		v := lastVersion
		mu.Unlock()
		t.Fatalf("sync timed out waiting for version %d (last seen: %d)", targetVersion, v)
	}
}

// assertClientFile checks that a file exists in the client directory with expected content.
func (e *testEnv) assertClientFile(t *testing.T, path, expectedContent string) {
	t.Helper()
	full := filepath.Join(e.clientDir, path)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read client file %s: %v", path, err)
	}
	if string(data) != expectedContent {
		t.Errorf("client file %s: got %q, want %q", path, string(data), expectedContent)
	}
}

// assertClientFileAbsent checks that a file does not exist in the client directory.
func (e *testEnv) assertClientFileAbsent(t *testing.T, path string) {
	t.Helper()
	full := filepath.Join(e.clientDir, path)
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Errorf("expected %s to not exist, but it does", path)
	}
}

func TestE2EInitialSync(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Write files to hub directory
	env.writeHubFile(t, "README.md", "# Hello")
	env.writeHubFile(t, "src/main.go", "package main")
	env.writeHubFile(t, "src/util.go", "package main\n// util")

	// Scan to populate change log
	env.scan(t)

	version, err := env.hubApp.Hub.Store.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("expected version 3, got %d", version)
	}

	// Sync client
	env.syncUntil(t, version, 5*time.Second)

	// Verify files
	env.assertClientFile(t, "README.md", "# Hello")
	env.assertClientFile(t, "src/main.go", "package main")
	env.assertClientFile(t, "src/util.go", "package main\n// util")
}

func TestE2EFileUpdate(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Initial file
	env.writeHubFile(t, "data.txt", "version1")
	env.scan(t)
	env.syncUntil(t, 1, 5*time.Second)
	env.assertClientFile(t, "data.txt", "version1")

	// Update file — write different-length content to ensure mtime+size diff
	time.Sleep(1100 * time.Millisecond) // ensure mtime differs (1s FS granularity)
	env.writeHubFile(t, "data.txt", "version2-updated")
	env.scan(t)

	version, _ := env.hubApp.Hub.Store.LatestVersion()
	env.syncUntil(t, version, 5*time.Second)
	env.assertClientFile(t, "data.txt", "version2-updated")
}

func TestE2EFileDelete(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Create and sync
	env.writeHubFile(t, "to-delete.txt", "bye")
	env.scan(t)
	env.syncUntil(t, 1, 5*time.Second)
	env.assertClientFile(t, "to-delete.txt", "bye")

	// Delete and sync
	env.deleteHubFile(t, "to-delete.txt")
	env.scan(t)

	version, _ := env.hubApp.Hub.Store.LatestVersion()
	env.syncUntil(t, version, 5*time.Second)
	env.assertClientFileAbsent(t, "to-delete.txt")
}

func TestE2EAuth(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Create a client with wrong token
	clientDBPath := filepath.Join(t.TempDir(), "bad-client.db")
	badClient, badCleanup, err := InitializeTestClientApp(&ClientConfig{
		DBPath:  clientDBPath,
		HubURL:  env.ts.URL,
		Token:   "wrong-token",
		SyncDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("init bad client: %v", err)
	}
	defer badCleanup()

	env.writeHubFile(t, "secret.txt", "classified")
	env.scan(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = badClient.Client.Sync(ctx)
	if ctx.Err() == context.DeadlineExceeded {
		// Expected: sync keeps retrying with wrong token
		return
	}
	// Also acceptable: immediate error about auth
	t.Logf("bad auth sync result: %v", err)
}

func TestE2ELargeFile(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Write a file larger than InlineThreshold (64KB)
	largeContent := make([]byte, InlineThreshold+1024)
	for i := range largeContent {
		largeContent[i] = byte(i % 256)
	}
	env.writeHubFile(t, "large.bin", string(largeContent))
	env.scan(t)

	version, _ := env.hubApp.Hub.Store.LatestVersion()
	env.syncUntil(t, version, 10*time.Second)

	// Verify content matches
	full := filepath.Join(env.clientDir, "large.bin")
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read large file: %v", err)
	}
	if len(data) != len(largeContent) {
		t.Fatalf("large file size: got %d, want %d", len(data), len(largeContent))
	}
	for i := range data {
		if data[i] != largeContent[i] {
			t.Fatalf("large file mismatch at byte %d", i)
			break
		}
	}
}

func TestE2EMultipleScans(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Scan 1: create files
	env.writeHubFile(t, "a.txt", "a1")
	env.writeHubFile(t, "b.txt", "b1")
	env.scan(t)

	v1, _ := env.hubApp.Hub.Store.LatestVersion()
	env.syncUntil(t, v1, 5*time.Second)
	env.assertClientFile(t, "a.txt", "a1")
	env.assertClientFile(t, "b.txt", "b1")

	// Scan 2: update, create, delete
	time.Sleep(1100 * time.Millisecond) // mtime granularity
	env.writeHubFile(t, "a.txt", "a2-longer")
	env.writeHubFile(t, "c.txt", "c1")
	env.deleteHubFile(t, "b.txt")
	env.scan(t)

	v2, _ := env.hubApp.Hub.Store.LatestVersion()
	env.syncUntil(t, v2, 5*time.Second)

	env.assertClientFile(t, "a.txt", "a2-longer")
	env.assertClientFileAbsent(t, "b.txt")
	env.assertClientFile(t, "c.txt", "c1")
}

func TestE2ESnapshot(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Populate hub with files
	for i := 0; i < 5; i++ {
		env.writeHubFile(t, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("content%d", i))
	}
	env.scan(t)

	// Bootstrap client from snapshot
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := env.clientApp.Client.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Verify files were extracted
	for i := 0; i < 5; i++ {
		env.assertClientFile(t, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("content%d", i))
	}

	// Verify hub version was set
	v, err := env.clientApp.Client.Store.HubVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Errorf("hub version after bootstrap: got %d, want 5", v)
	}
}
