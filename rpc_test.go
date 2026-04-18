package hubsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestRPCServer(t *testing.T, env *reconcilerEnv) (*RPCClient, context.CancelFunc) {
	t.Helper()
	sock := shortSocketPath(t)
	srv := &RPCServer{
		Reconciler: env.recon,
		Store:      env.store,
		Socket:     sock,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	// Wait for socket to show up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return NewRPCClient(sock, ""), cancel
}

func TestRPCPinUnpinLsStatus(t *testing.T) {
	env := newReconcilerEnv(t)
	env.writeLocal(t, "a.txt", "alpha")
	env.appendEntry(t, "a.txt", "alpha")
	env.writeLocal(t, "dir/b.txt", "beta")
	env.appendEntry(t, "dir/b.txt", "beta")

	client, _ := newTestRPCServer(t, env)
	ctx := context.Background()

	// Status before pinning — everything NULL.
	st, err := client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Null.Count != 2 {
		t.Errorf("expected 2 NULL rows, got %d", st.Null.Count)
	}

	// Pin everything.
	pin, err := client.Pin(ctx, PinRequest{Globs: []string{"**/*.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pin.Results) != 2 {
		t.Fatalf("expected 2 pin results, got %d", len(pin.Results))
	}
	for _, r := range pin.Results {
		if r.Error != "" {
			t.Errorf("pin %s error: %s", r.Path, r.Error)
		}
	}

	// ls should show both as archived, with the thin-projection JSONL
	// fields: path, kind, digest, size, mtime, archive_state, etc.
	ls, err := client.Ls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.Entries) != 2 {
		t.Fatalf("ls got %d entries", len(ls.Entries))
	}
	for _, e := range ls.Entries {
		if e.ArchiveState != "archived" {
			t.Errorf("%s archive_state=%q", e.Path, e.ArchiveState)
		}
		if e.Kind != "file" {
			t.Errorf("%s kind=%q want file", e.Path, e.Kind)
		}
		if e.Digest == "" {
			t.Errorf("%s digest is empty", e.Path)
		}
		if e.Size == 0 {
			t.Errorf("%s size is 0", e.Path)
		}
	}

	// Dry-run unpin of dir/b.txt: plan only, no state change.
	dry, err := client.Unpin(ctx, PinRequest{Globs: []string{"dir/b.txt"}, Dry: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Results) != 1 || !dry.Results[0].Dry {
		t.Fatalf("dry-run result = %+v", dry.Results)
	}
	if _, err := os.Stat(filepath.Join(env.hubDir, "dir/b.txt")); err != nil {
		t.Errorf("dry-run must not unlink local file; stat err=%v", err)
	}

	// Actual unpin — should drop the local file.
	unpin, err := client.Unpin(ctx, PinRequest{Globs: []string{"dir/b.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range unpin.Results {
		if r.Error != "" {
			t.Errorf("unpin %s error: %s", r.Path, r.Error)
		}
	}
	if _, err := os.Stat(filepath.Join(env.hubDir, "dir/b.txt")); !os.IsNotExist(err) {
		t.Errorf("local file should be removed, err=%v", err)
	}

	// Status after: 1 archived, 1 unpinned.
	st2, err := client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Archive.Count != 1 {
		t.Errorf("archived count=%d want 1", st2.Archive.Count)
	}
	if st2.Unpinned.Count != 1 {
		t.Errorf("unpinned count=%d want 1", st2.Unpinned.Count)
	}
}

func TestRPCAuthRequiredWhenTokenSet(t *testing.T) {
	env := newReconcilerEnv(t)
	env.writeLocal(t, "a.txt", "x")
	env.appendEntry(t, "a.txt", "x")

	sock := shortSocketPath(t)
	srv := &RPCServer{
		Reconciler: env.recon,
		Store:      env.store,
		Socket:     sock,
		Token:      BearerToken("secret"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() { cancel(); <-done })

	// Client without token should fail.
	bad := NewRPCClient(sock, "")
	if _, err := bad.Status(ctx); err == nil {
		t.Fatal("expected auth failure without token")
	}

	// With token it works.
	good := NewRPCClient(sock, "secret")
	if _, err := good.Status(ctx); err != nil {
		t.Fatalf("good client: %v", err)
	}
}

func TestRPCNoServeRunning(t *testing.T) {
	c := NewRPCClient(filepath.Join(t.TempDir(), "missing.sock"), "")
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected error when socket missing")
	}
	if !containsString(err.Error(), "no hubsync serve running") {
		t.Errorf("error should hint at missing serve: %v", err)
	}
}

// shortSocketPath returns a unix-socket path short enough for macOS's
// 104-byte sun_path limit. t.TempDir() paths under /var/folders can blow
// that out.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && indexOf(s, sub) >= 0))
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
