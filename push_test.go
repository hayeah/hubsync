package hubsync

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestE2EPushBasic(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Initial sync: hub has a file
	env.writeHubFile(t, "hub.txt", "from hub")
	env.scan(t)
	env.syncUntil(t, 1, 5*time.Second)
	env.assertClientFile(t, "hub.txt", "from hub")

	// Client creates a new file locally
	writeClientFile(t, env.clientDir, "client.txt", "from client")

	// Push from client
	accepted, err := env.clientApp.Client.Push(ConflictPolicy_FAIL)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected 1 accepted, got %d", accepted)
	}

	// Verify file appeared on hub
	assertHubFile(t, env.hubDir, "client.txt", "from client")
}

func TestE2EPushUpdate(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Hub creates a file, client syncs
	env.writeHubFile(t, "shared.txt", "original")
	env.scan(t)
	env.syncUntil(t, 1, 5*time.Second)
	env.assertClientFile(t, "shared.txt", "original")

	// Client modifies the file
	writeClientFile(t, env.clientDir, "shared.txt", "modified by client")

	// Push update
	accepted, err := env.clientApp.Client.Push(ConflictPolicy_FAIL)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected 1 accepted, got %d", accepted)
	}

	// Verify hub has the update
	assertHubFile(t, env.hubDir, "shared.txt", "modified by client")
}

func TestE2EPushDelete(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Hub creates a file, client syncs
	env.writeHubFile(t, "to-delete.txt", "delete me")
	env.scan(t)
	env.syncUntil(t, 1, 5*time.Second)
	env.assertClientFile(t, "to-delete.txt", "delete me")

	// Client deletes the file
	os.Remove(filepath.Join(env.clientDir, "to-delete.txt"))

	// Push deletion
	accepted, err := env.clientApp.Client.Push(ConflictPolicy_FAIL)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected 1 accepted, got %d", accepted)
	}

	// Verify hub no longer has the file
	hubPath := filepath.Join(env.hubDir, "to-delete.txt")
	if _, err := os.Stat(hubPath); !os.IsNotExist(err) {
		t.Errorf("expected hub file to be deleted")
	}
}

func TestE2EPushConflictedCopy(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Hub creates a file, client syncs
	env.writeHubFile(t, "conflict.txt", "original")
	env.scan(t)
	env.syncUntil(t, 1, 5*time.Second)

	// Hub modifies the file (without client knowing)
	time.Sleep(1100 * time.Millisecond)
	env.writeHubFile(t, "conflict.txt", "hub modified")
	env.scan(t)

	// Client modifies the same file (will conflict)
	writeClientFile(t, env.clientDir, "conflict.txt", "client modified")

	// Push should report conflict
	accepted, err := env.clientApp.Client.Push(ConflictPolicy_FAIL)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if accepted != 0 {
		t.Fatalf("expected 0 accepted (conflict), got %d", accepted)
	}

	// Hub should still have its version
	assertHubFile(t, env.hubDir, "conflict.txt", "hub modified")

	// Client's local version should be renamed to conflicted copy
	conflictedPath := filepath.Join(env.clientDir, "conflict (conflicted copy).txt")
	data, err := os.ReadFile(conflictedPath)
	if err != nil {
		t.Fatalf("expected conflicted copy at %s: %v", conflictedPath, err)
	}
	if string(data) != "client modified" {
		t.Errorf("conflicted copy content: got %q, want %q", string(data), "client modified")
	}

	// Original path should now have hub's version (fetched after conflict)
	env.assertClientFile(t, "conflict.txt", "hub modified")
}

func TestE2EPushClientWins(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Hub creates a file, client syncs
	env.writeHubFile(t, "force.txt", "original")
	env.scan(t)
	env.syncUntil(t, 1, 5*time.Second)

	// Hub modifies
	time.Sleep(1100 * time.Millisecond)
	env.writeHubFile(t, "force.txt", "hub version")
	env.scan(t)

	// Client modifies
	writeClientFile(t, env.clientDir, "force.txt", "client wins")

	// Push with CLIENT_WINS
	accepted, err := env.clientApp.Client.Push(ConflictPolicy_CLIENT_WINS)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("expected 1 accepted, got %d", accepted)
	}

	assertHubFile(t, env.hubDir, "force.txt", "client wins")
}

func TestE2EWriteMode(t *testing.T) {
	env := newTestEnv(t)
	defer env.cleanup()

	// Enable write mode on client with short scan interval
	env.clientApp.Client.SetWriteMode(500 * time.Millisecond)

	// Hub creates initial files
	env.writeHubFile(t, "hub-file.txt", "from hub")
	env.scan(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var lastVersion int64
	pushDone := make(chan struct{}, 10)

	env.clientApp.Client.OnEvent = func(version int64, path string, op ChangeOp) {
		mu.Lock()
		lastVersion = version
		mu.Unlock()
	}

	env.clientApp.Client.OnPush = func(accepted int) {
		if accepted > 0 {
			pushDone <- struct{}{}
		}
	}

	// Start sync in background
	go env.clientApp.Client.Sync(ctx)

	// Wait for initial sync
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lastVersion >= 1
	})

	env.assertClientFile(t, "hub-file.txt", "from hub")

	// Write a file on the client side
	writeClientFile(t, env.clientDir, "client-created.txt", "hello from client")

	// Wait for push to complete
	select {
	case <-pushDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for push")
	}

	// Verify file appeared on hub
	assertHubFile(t, env.hubDir, "client-created.txt", "hello from client")

	cancel()
}

// --- helpers ---

func writeClientFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func assertHubFile(t *testing.T, dir, path, expected string) {
	t.Helper()
	full := filepath.Join(dir, path)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read hub file %s: %v", path, err)
	}
	if string(data) != expected {
		t.Errorf("hub file %s: got %q, want %q", path, string(data), expected)
	}
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("waitFor timed out")
}
