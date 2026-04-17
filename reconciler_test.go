package hubsync

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hayeah/hubsync/archive"
)

type reconcilerEnv struct {
	hubDir  string
	store   *HubStore
	storage *archive.FakeStorage
	recon   *Reconciler
}

func newReconcilerEnv(t *testing.T) *reconcilerEnv {
	t.Helper()
	hubDir := t.TempDir()
	store, cleanup, err := NewHubStore(HubStoreConfig{
		DBPath: filepath.Join(hubDir, "hub.db"),
		Hasher: sha256Hasher{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	fake := archive.NewFakeStorage()
	return &reconcilerEnv{
		hubDir:  hubDir,
		store:   store,
		storage: fake,
		recon: &Reconciler{
			Store:   store,
			Storage: fake,
			Hasher:  sha256Hasher{},
			HubDir:  hubDir,
			Prefix:  "backups/test/",
		},
	}
}

func (e *reconcilerEnv) writeLocal(t *testing.T, path, content string) {
	t.Helper()
	full := filepath.Join(e.hubDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func (e *reconcilerEnv) appendEntry(t *testing.T, path, content string) {
	t.Helper()
	st, err := os.Stat(filepath.Join(e.hubDir, path))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256Hasher{}.Sum([]byte(content))
	_, err = e.store.Append(ChangeEntry{
		Path:   path,
		Op:     OpCreate,
		Kind:   FileKindFile,
		Digest: digest,
		Size:   int64(len(content)),
		Mode:   uint32(st.Mode().Perm()),
		MTime:  st.ModTime().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReconcilerPlanPinStates(t *testing.T) {
	env := newReconcilerEnv(t)
	env.writeLocal(t, "a.txt", "a")
	env.appendEntry(t, "a.txt", "a") // NULL state

	cases := []struct {
		setup func()
		want  []PlanStep
	}{
		{want: []PlanStep{StepArchive}}, // NULL
		{
			setup: func() { env.store.MarkArchived("a.txt", "fid", nil, 0) },
			want:  []PlanStep{StepNoOp},
		},
		{
			setup: func() { env.store.MarkUnpinned("a.txt") },
			want:  []PlanStep{StepRestore},
		},
		{
			setup: func() { env.store.MarkArchiveDirty("a.txt") },
			want:  []PlanStep{StepArchive},
		},
	}
	for i, tc := range cases {
		if tc.setup != nil {
			tc.setup()
		}
		p, err := env.recon.PlanPin("a.txt")
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !reflect.DeepEqual(p.Steps, tc.want) {
			t.Errorf("case %d: got %v, want %v", i, p.Steps, tc.want)
		}
	}
}

func TestReconcilerPlanUnpinStates(t *testing.T) {
	env := newReconcilerEnv(t)
	env.writeLocal(t, "a.txt", "a")
	env.appendEntry(t, "a.txt", "a") // NULL

	cases := []struct {
		setup func()
		want  []PlanStep
	}{
		{want: []PlanStep{StepArchive, StepEvict}}, // NULL
		{
			setup: func() { env.store.MarkArchiveDirty("a.txt") },
			want:  []PlanStep{StepArchive, StepEvict},
		},
		{
			setup: func() { env.store.MarkArchived("a.txt", "fid", nil, 0) },
			want:  []PlanStep{StepEvict},
		},
		{
			setup: func() { env.store.MarkUnpinned("a.txt") },
			want:  []PlanStep{StepNoOp},
		},
	}
	for i, tc := range cases {
		if tc.setup != nil {
			tc.setup()
		}
		p, err := env.recon.PlanUnpin("a.txt")
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !reflect.DeepEqual(p.Steps, tc.want) {
			t.Errorf("case %d: got %v, want %v", i, p.Steps, tc.want)
		}
	}
}

func TestReconcilerFullPinUnpinCycle(t *testing.T) {
	env := newReconcilerEnv(t)
	env.writeLocal(t, "photo.jpg", "binary-photo-bytes")
	env.appendEntry(t, "photo.jpg", "binary-photo-bytes")

	ctx := context.Background()

	// Pin (NULL → archived).
	plan, err := env.recon.PlanPin("photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.recon.Apply(ctx, plan); err != nil {
		t.Fatalf("pin apply: %v", err)
	}
	e1, _, _ := env.store.EntryLookup("photo.jpg")
	if e1.ArchiveState != ArchiveStateArchived {
		t.Fatalf("after pin, state=%q", e1.ArchiveState)
	}
	if e1.ArchiveFileID == "" {
		t.Error("archive_file_id not set")
	}

	// Unpin (archived → unpinned). Local file should disappear.
	plan2, err := env.recon.PlanUnpin("photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.recon.Apply(ctx, plan2); err != nil {
		t.Fatalf("unpin apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.hubDir, "photo.jpg")); !os.IsNotExist(err) {
		t.Errorf("local file should be gone, err=%v", err)
	}
	e2, _, _ := env.store.EntryLookup("photo.jpg")
	if e2.ArchiveState != ArchiveStateUnpinned {
		t.Errorf("after unpin, state=%q", e2.ArchiveState)
	}
	if e2.ArchiveFileID == "" {
		t.Error("archive_file_id should persist through unpin")
	}

	// Pin again (unpinned → archived via restore). Local file should reappear.
	plan3, err := env.recon.PlanPin("photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if plan3.Steps[0] != StepRestore {
		t.Fatalf("expected restore step, got %v", plan3.Steps)
	}
	if err := env.recon.Apply(ctx, plan3); err != nil {
		t.Fatalf("restore apply: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(env.hubDir, "photo.jpg"))
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(data) != "binary-photo-bytes" {
		t.Errorf("restored content = %q", data)
	}
	e3, _, _ := env.store.EntryLookup("photo.jpg")
	if e3.ArchiveState != ArchiveStateArchived {
		t.Errorf("after re-pin, state=%q", e3.ArchiveState)
	}
}

func TestReconcilerEvictAbortsOnRemoteMismatch(t *testing.T) {
	env := newReconcilerEnv(t)
	env.writeLocal(t, "a.txt", "a")
	env.appendEntry(t, "a.txt", "a")
	// Pretend the row is archived but pointing at a stale fileId.
	if err := env.store.MarkArchived("a.txt", "wrong-fileId", nil, 0); err != nil {
		t.Fatal(err)
	}
	// Storage has no upload for this path at all.
	err := env.recon.Evict(context.Background(), "a.txt")
	if err == nil {
		t.Fatal("expected Evict to fail when remote is missing/stale")
	}
	// Local file should still exist (we aborted before unlink).
	if _, err := os.Stat(filepath.Join(env.hubDir, "a.txt")); err != nil {
		t.Errorf("local file should survive aborted evict, stat err=%v", err)
	}
}

func TestReconcilerPinSeedsNewPath(t *testing.T) {
	env := newReconcilerEnv(t)
	// File on disk but no hub_entry row yet.
	env.writeLocal(t, "fresh.txt", "fresh-bytes")

	plan, err := env.recon.PlanPin("fresh.txt")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0] != StepSeed {
		t.Fatalf("expected seed step, got %v", plan.Steps)
	}
	if err := env.recon.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	e, ok, _ := env.store.EntryLookup("fresh.txt")
	if !ok || e.ArchiveState != ArchiveStateArchived {
		t.Fatalf("row not seeded+archived; state=%q ok=%v", e.ArchiveState, ok)
	}
}
