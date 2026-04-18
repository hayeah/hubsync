package hubsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayeah/hubsync/archive"
)

type archiveGCEnv struct {
	store   *HubStore
	storage *archive.FakeStorage
	gc      *ArchiveGC
	prefix  string
}

func newArchiveGCEnv(t *testing.T) *archiveGCEnv {
	t.Helper()
	store, cleanup, err := NewHubStore(HubStoreConfig{
		DBPath: filepath.Join(t.TempDir(), "hub.db"),
		Hasher: sha256Hasher{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	fake := archive.NewFakeStorage()
	prefix := "b2cast-test/2026-04-17/"
	return &archiveGCEnv{
		store:   store,
		storage: fake,
		prefix:  prefix,
		gc: &ArchiveGC{
			Store:   store,
			Storage: fake,
			Prefix:  prefix,
		},
	}
}

// upload a remote object at <prefix>+relPath.
func (e *archiveGCEnv) uploadRemote(t *testing.T, relPath, body string) archive.RemoteInfo {
	t.Helper()
	info, err := e.storage.Upload(context.Background(), archive.UploadRequest{
		Key:    e.prefix + relPath,
		Size:   int64(len(body)),
		Source: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// uploadBare uploads to an absolute bucket key (no hub prefix). Used to
// simulate objects sitting outside the hub's jurisdiction.
func (e *archiveGCEnv) uploadBare(t *testing.T, key, body string) archive.RemoteInfo {
	t.Helper()
	info, err := e.storage.Upload(context.Background(), archive.UploadRequest{
		Key:    key,
		Size:   int64(len(body)),
		Source: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// claim registers a hub_entry row for relPath with archive_state='archived'
// and a matching fileID, so claim-check sees it.
func (e *archiveGCEnv) claim(t *testing.T, relPath, body, fileID string) {
	t.Helper()
	_, err := e.store.Append(ChangeEntry{
		Path:   relPath,
		Op:     OpCreate,
		Kind:   FileKindFile,
		Digest: sha256Hasher{}.Sum([]byte(body)),
		Size:   int64(len(body)),
		Mode:   0644,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkArchived(relPath, fileID, nil, 0); err != nil {
		t.Fatal(err)
	}
}

func collectEntries(t *testing.T, env *archiveGCEnv, prefix string, dry bool) ([]ArchiveGCEntry, ArchiveGCSummary) {
	t.Helper()
	var got []ArchiveGCEntry
	summary, err := env.gc.Run(context.Background(), prefix, dry, func(e ArchiveGCEntry) {
		got = append(got, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, summary
}

func TestArchiveGC_OrphanFileAtTopLevel(t *testing.T) {
	env := newArchiveGCEnv(t)
	info := env.uploadRemote(t, "stray.txt", "hello")

	// dry run: classify without deleting
	entries, summary := collectEntries(t, env, env.prefix, true)

	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Kind != "file" || e.Action != ArchiveGCDelete || e.Claimed {
		t.Errorf("unexpected classification: %+v", e)
	}
	if e.Deleted {
		t.Error("--dry must not delete")
	}
	if e.Path != "stray.txt" {
		t.Errorf("path=%q want stray.txt", e.Path)
	}
	if summary.OrphanFiles != 1 || summary.DeletedFiles != 0 {
		t.Errorf("summary=%+v", summary)
	}

	// real run: deletes
	if env.storage.VersionCount(info.Key) != 1 {
		t.Fatal("setup: expected one version")
	}
	entries, summary = collectEntries(t, env, env.prefix, false)
	if len(entries) != 1 || !entries[0].Deleted {
		t.Fatalf("want one deleted entry, got %+v", entries)
	}
	if env.storage.VersionCount(info.Key) != 0 {
		t.Error("real run should have removed the B2 object")
	}
	if summary.DeletedFiles != 1 || summary.DeletedBytes != 5 {
		t.Errorf("summary=%+v", summary)
	}
}

func TestArchiveGC_OrphanDirDeleteSubtree(t *testing.T) {
	env := newArchiveGCEnv(t)
	// Three files under an unclaimed sid/.
	env.uploadRemote(t, "sid-orphan/a.bin", "aa")
	env.uploadRemote(t, "sid-orphan/b.bin", "bbb")
	env.uploadRemote(t, "sid-orphan/sub/c.bin", "c")

	entries, summary := collectEntries(t, env, env.prefix, false)
	if len(entries) != 1 {
		t.Fatalf("want 1 top-level entry (the dir), got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Kind != "dir" || e.Action != ArchiveGCDeleteSubtree || e.Claimed {
		t.Errorf("unexpected dir classification: %+v", e)
	}
	if !e.Deleted || e.DeletedN != 3 {
		t.Errorf("want Deleted=true DeletedN=3, got %+v", e)
	}
	if summary.OrphanDirs != 1 || summary.DeletedFiles != 3 {
		t.Errorf("summary=%+v", summary)
	}
	// Underlying objects gone.
	for _, k := range []string{
		env.prefix + "sid-orphan/a.bin",
		env.prefix + "sid-orphan/b.bin",
		env.prefix + "sid-orphan/sub/c.bin",
	} {
		if env.storage.VersionCount(k) != 0 {
			t.Errorf("object %s should be gone", k)
		}
	}
}

func TestArchiveGC_MixedClaimDirKept(t *testing.T) {
	env := newArchiveGCEnv(t)
	// sid-kept has one claimed file and one unclaimed file — must keep the
	// whole sid at this level; operator would re-run at sid-kept/ for
	// file-level scrutiny.
	info := env.uploadRemote(t, "sid-kept/claimed.bin", "x")
	_ = env.uploadRemote(t, "sid-kept/orphan.bin", "y")
	env.claim(t, "sid-kept/claimed.bin", "x", info.FileID)

	entries, summary := collectEntries(t, env, env.prefix, false)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry (the dir), got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Kind != "dir" || e.Action != ArchiveGCKeep || !e.Claimed {
		t.Errorf("want kept claimed dir, got %+v", e)
	}
	// No deletes at all.
	if env.storage.VersionCount(env.prefix+"sid-kept/claimed.bin") != 1 ||
		env.storage.VersionCount(env.prefix+"sid-kept/orphan.bin") != 1 {
		t.Error("no deletes should have happened inside a kept dir")
	}
	if summary.KeptDirs != 1 || summary.DeletedFiles != 0 {
		t.Errorf("summary=%+v", summary)
	}
}

func TestArchiveGC_FileIDGuardSkipsDeleteOnRace(t *testing.T) {
	env := newArchiveGCEnv(t)
	// Orphan file at t=0; we capture its fileID via list.
	firstInfo := env.uploadRemote(t, "race.bin", "v1")
	_ = firstInfo

	// Simulate a race: before archive-gc's delete fires, a new version is
	// uploaded at the same key (fileID changes).
	env.uploadRemote(t, "race.bin", "v2")

	// Prime the gc by running list → the entry it captured has firstInfo's
	// fileID (stale). The easiest way to simulate this is to call Run
	// AFTER the race, which lists the current head (v2). To reproduce the
	// stale-fileID case deterministically we bypass the Run driver and
	// manually exercise DeleteByKey with the stale fileID.
	err := env.storage.DeleteByKey(context.Background(), env.prefix+"race.bin", firstInfo.FileID)
	if err == nil {
		t.Fatal("want fileID-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "fileID mismatch") {
		t.Errorf("want fileID-mismatch, got %v", err)
	}
	// Both versions still exist (delete was aborted).
	if env.storage.VersionCount(env.prefix+"race.bin") == 0 {
		t.Error("delete should have been aborted by fileID guard")
	}
}

func TestArchiveGC_OutsideHubPrefixIsOrphan(t *testing.T) {
	env := newArchiveGCEnv(t)
	// Operator runs archive-gc against a prefix the hub doesn't claim at
	// all (the b2cast-saturate case). Every entry is orphan.
	env.uploadBare(t, "b2cast-saturate/2026-04-17/obj1", "aa")
	env.uploadBare(t, "b2cast-saturate/2026-04-17/obj2", "bb")

	entries, summary := collectEntries(t, env, "b2cast-saturate/2026-04-17/", false)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Action != ArchiveGCDelete || e.Claimed {
			t.Errorf("want unclaimed delete for %+v", e)
		}
		if e.Path != "" {
			t.Errorf("Path should be empty for out-of-jurisdiction keys: %q", e.Path)
		}
		if !e.Deleted {
			t.Errorf("expected deleted, got %+v", e)
		}
	}
	if summary.OrphanFiles != 2 || summary.DeletedFiles != 2 {
		t.Errorf("summary=%+v", summary)
	}
}

func TestArchiveGC_DryRunMutatesNothing(t *testing.T) {
	env := newArchiveGCEnv(t)
	env.uploadRemote(t, "orphan.bin", "x")
	env.uploadRemote(t, "sid-orphan/a.bin", "y")

	_, _ = collectEntries(t, env, env.prefix, true)

	if env.storage.VersionCount(env.prefix+"orphan.bin") != 1 ||
		env.storage.VersionCount(env.prefix+"sid-orphan/a.bin") != 1 {
		t.Error("dry run must not delete any objects")
	}
}
