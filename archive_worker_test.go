package hubsync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hayeah/hubsync/archive"
)

type workerEnv struct {
	hubDir  string
	store   *HubStore
	storage *archive.FakeStorage
	worker  *ArchiveWorker
}

func newWorkerEnv(t *testing.T) *workerEnv {
	t.Helper()
	hubDir := t.TempDir()
	dbPath := filepath.Join(hubDir, "hub.db")
	store, cleanup, err := NewHubStore(HubStoreConfig{DBPath: dbPath, Hasher: sha256Hasher{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	fake := archive.NewFakeStorage()
	return &workerEnv{
		hubDir:  hubDir,
		store:   store,
		storage: fake,
		worker: &ArchiveWorker{
			Store:   store,
			Storage: fake,
			Hasher:  sha256Hasher{},
			HubDir:  hubDir,
			Prefix:  "backups/test/",
			Workers: 2,
		},
	}
}

// stampLocalAndEntry writes a file under hubDir and appends a hub_entry row.
func (e *workerEnv) stampLocalAndEntry(t *testing.T, path, content string) {
	t.Helper()
	full := filepath.Join(e.hubDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	digest := sha256Hasher{}.Sum([]byte(content))
	if _, err := e.store.Append(ChangeEntry{
		Path:   path,
		Op:     OpCreate,
		Kind:   FileKindFile,
		Digest: digest,
		Size:   int64(len(content)),
		Mode:   0644,
		MTime:  time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveWorkerBaselineUploads(t *testing.T) {
	env := newWorkerEnv(t)
	env.stampLocalAndEntry(t, "a.txt", "alpha")
	env.stampLocalAndEntry(t, "dir/b.txt", "beta")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Run() blocks; run it in a goroutine and wait for the baseline to drain.
	errCh := make(chan error, 1)
	go func() { errCh <- env.worker.Run(ctx) }()

	waitForCondition(t, func() bool {
		e, _, _ := env.store.EntryLookup("a.txt")
		e2, _, _ := env.store.EntryLookup("dir/b.txt")
		return e.ArchiveState == ArchiveStateArchived && e2.ArchiveState == ArchiveStateArchived
	}, 2*time.Second)

	// Remote should have both uploads.
	if got, _ := env.storage.Bytes("backups/test/a.txt"); string(got) != "alpha" {
		t.Errorf("remote a.txt = %q want %q", got, "alpha")
	}
	if got, _ := env.storage.Bytes("backups/test/dir/b.txt"); string(got) != "beta" {
		t.Errorf("remote dir/b.txt = %q want %q", got, "beta")
	}

	// hub_entry should carry archive metadata.
	got, _, _ := env.store.EntryLookup("a.txt")
	if got.ArchiveFileID == "" {
		t.Error("archive_file_id not recorded")
	}
	if got.ArchiveUploadedAt == 0 {
		t.Error("archive_uploaded_at not recorded")
	}

	cancel()
	<-errCh
}

func TestArchiveWorkerBroadcastPicksUpNewRow(t *testing.T) {
	env := newWorkerEnv(t)
	bc := NewBroadcaster()
	env.worker.Broadcaster = bc

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- env.worker.Run(ctx) }()

	// Baseline is empty; write a file afterwards and broadcast.
	env.stampLocalAndEntry(t, "late.txt", "hello-late")
	bc.Broadcast(ChangeEntry{Path: "late.txt", Op: OpCreate, Kind: FileKindFile})

	waitForCondition(t, func() bool {
		e, _, _ := env.store.EntryLookup("late.txt")
		return e.ArchiveState == ArchiveStateArchived
	}, 2*time.Second)

	got, _ := env.storage.Bytes("backups/test/late.txt")
	if !bytes.Equal(got, []byte("hello-late")) {
		t.Errorf("remote late.txt = %q", got)
	}

	cancel()
	<-errCh
}

func TestArchiveWorkerSkipsAlreadyArchived(t *testing.T) {
	env := newWorkerEnv(t)
	env.stampLocalAndEntry(t, "done.txt", "ok")
	// Mark archived already (simulate prior run).
	if err := env.store.MarkArchived("done.txt", "pre-fileId", nil, 0); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- env.worker.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-errCh

	// No upload should have happened — PendingArchiveRows skips archived.
	if c := env.storage.VersionCount("backups/test/done.txt"); c != 0 {
		t.Errorf("expected 0 uploads for already-archived row, got %d", c)
	}
	got, _, _ := env.store.EntryLookup("done.txt")
	if got.ArchiveFileID != "pre-fileId" {
		t.Errorf("fileId got overwritten: %q", got.ArchiveFileID)
	}
}

// waitFor polls the condition up to d; fails the test if it doesn't become
// true in time.
func waitForCondition(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition did not become true within %v", d)
}
