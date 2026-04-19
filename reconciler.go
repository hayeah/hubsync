package hubsync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hayeah/hubsync/archive"
)

// Reconciler drives declarative `pin` / `unpin` commands by composing three
// primitives — archive, evict, restore — against hub_entry rows. It runs
// inside the serve process (single writer), so there's no concurrent
// state-change to the same row.
type Reconciler struct {
	Store   *HubStore
	Storage archive.ArchiveStorage
	Hasher  Hasher
	HubDir  string // absolute path; files resolved as HubDir/<path>
	Prefix  string // bucket_prefix; keys computed as Prefix + <path>
}

// TargetState describes what a reconciler command is trying to reach.
type TargetState string

const (
	TargetArchived TargetState = "archived"
	TargetUnpinned TargetState = "unpinned"
)

// PlanStep is one ordered action the reconciler intends to run to move a path
// toward its target. The CLI's --dry flag prints these without executing.
type PlanStep string

const (
	StepArchive PlanStep = "archive"
	StepEvict   PlanStep = "evict"
	StepRestore PlanStep = "restore"
	StepSeed    PlanStep = "seed" // seed a hub_entry row for a local file not yet tracked
	StepNoOp    PlanStep = "noop"
)

// Plan is the ordered steps for one path.
type Plan struct {
	Path  string
	Steps []PlanStep
	// StartingState is the hub_entry's archive_state at plan time (empty
	// string for NULL, "∅" for no row at all).
	StartingState string
}

// PlanPin returns the steps needed to drive path to the archived state.
func (r *Reconciler) PlanPin(path string) (Plan, error) {
	entry, ok, err := r.Store.EntryLookup(path)
	if err != nil {
		return Plan{}, err
	}
	if !ok {
		if _, err := os.Stat(filepath.Join(r.HubDir, path)); err != nil {
			return Plan{Path: path, StartingState: "∅"}, fmt.Errorf("pin: no such path %q", path)
		}
		return Plan{Path: path, StartingState: "∅", Steps: []PlanStep{StepSeed, StepArchive}}, nil
	}
	p := Plan{Path: path, StartingState: string(entry.ArchiveState)}
	switch entry.ArchiveState {
	case ArchiveStateArchived:
		p.Steps = []PlanStep{StepNoOp}
	case ArchiveStateUnpinned:
		p.Steps = []PlanStep{StepRestore}
	default: // NULL or dirty
		p.Steps = []PlanStep{StepArchive}
	}
	return p, nil
}

// PlanUnpin returns the steps needed to drive path to the unpinned state.
func (r *Reconciler) PlanUnpin(path string) (Plan, error) {
	entry, ok, err := r.Store.EntryLookup(path)
	if err != nil {
		return Plan{}, err
	}
	if !ok {
		if _, err := os.Stat(filepath.Join(r.HubDir, path)); err != nil {
			return Plan{Path: path, StartingState: "∅"}, fmt.Errorf("unpin: no such path %q", path)
		}
		return Plan{Path: path, StartingState: "∅", Steps: []PlanStep{StepSeed, StepArchive, StepEvict}}, nil
	}
	p := Plan{Path: path, StartingState: string(entry.ArchiveState)}
	switch entry.ArchiveState {
	case ArchiveStateUnpinned:
		p.Steps = []PlanStep{StepNoOp}
	case ArchiveStateArchived:
		p.Steps = []PlanStep{StepEvict}
	default: // NULL or dirty
		p.Steps = []PlanStep{StepArchive, StepEvict}
	}
	return p, nil
}

// Apply runs the plan to completion, committing each step before moving on.
// It is safe to call inside the archive worker's goroutine (single writer).
func (r *Reconciler) Apply(ctx context.Context, plan Plan) error {
	for _, step := range plan.Steps {
		switch step {
		case StepNoOp:
			// already at target
		case StepSeed:
			if err := r.seedRow(plan.Path); err != nil {
				return fmt.Errorf("seed %s: %w", plan.Path, err)
			}
		case StepArchive:
			if err := r.Archive(ctx, plan.Path); err != nil {
				return fmt.Errorf("archive %s: %w", plan.Path, err)
			}
		case StepEvict:
			if err := r.Evict(ctx, plan.Path); err != nil {
				return fmt.Errorf("evict %s: %w", plan.Path, err)
			}
		case StepRestore:
			if err := r.Restore(ctx, plan.Path); err != nil {
				return fmt.Errorf("restore %s: %w", plan.Path, err)
			}
		default:
			return fmt.Errorf("unknown plan step: %s", step)
		}
	}
	return nil
}

// Archive uploads the file at path to B2 and flips archive_state to
// 'archived'. Safe on NULL and dirty rows; no-op on already-archived.
func (r *Reconciler) Archive(ctx context.Context, path string) error {
	entry, ok, err := r.Store.EntryLookup(path)
	if err != nil {
		return err
	}
	if !ok || entry.Kind != FileKindFile {
		return fmt.Errorf("archive: no file row for %q", path)
	}
	if entry.ArchiveState == ArchiveStateArchived {
		return nil
	}
	fullPath := filepath.Join(r.HubDir, path)
	src, err := os.Open(fullPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", fullPath, err)
	}
	defer src.Close()

	info, err := r.Storage.Upload(ctx, archive.UploadRequest{
		Key:        archive.JoinKey(r.Prefix, path),
		Size:       entry.Size,
		Source:     src,
		Digest:     entry.Digest.Bytes(),
		DigestAlgo: r.Hasher.Name(),
	})
	if err != nil {
		return err
	}
	sha1Bytes, _ := hex.DecodeString(info.ContentSHA1)
	return r.Store.MarkArchived(path, info.FileID, sha1Bytes, info.UploadedAt.UnixMilli())
}

// Evict re-verifies that the remote head matches hub_entry's recorded
// archive_file_id + digest, flips archive_state to 'unpinned' in a
// transaction, then removes the local file. Order matters: commit BEFORE
// unlinking so the watcher callback reads the post-change row and swallows
// the fsnotify delete.
func (r *Reconciler) Evict(ctx context.Context, path string) error {
	entry, ok, err := r.Store.EntryLookup(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("evict: no row for %q", path)
	}
	if entry.ArchiveState != ArchiveStateArchived {
		return fmt.Errorf("evict: %q is in state %q; need 'archived'", path, entry.ArchiveState)
	}

	key := archive.JoinKey(r.Prefix, path)
	head, err := r.Storage.HeadByKey(ctx, key)
	if err != nil {
		return fmt.Errorf("evict head-check %s: %w", key, err)
	}
	if head.FileID != entry.ArchiveFileID {
		return fmt.Errorf("evict: remote head fileId=%q differs from recorded %q", head.FileID, entry.ArchiveFileID)
	}
	if head.Size != entry.Size {
		return fmt.Errorf("evict: remote size=%d differs from recorded %d", head.Size, entry.Size)
	}
	if !bytes.Equal(head.Digest, entry.Digest.Bytes()) {
		return fmt.Errorf("evict: remote digest differs from recorded")
	}

	if err := r.Store.MarkUnpinned(path); err != nil {
		return err
	}
	fullPath := filepath.Join(r.HubDir, path)
	if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove local %s: %w", fullPath, err)
	}
	return nil
}

// Restore downloads the archived version for path, verifies the digest
// matches hub_entry.digest, flips state to 'archived' in a transaction, then
// renames the tempfile into place. Order matters for the same reason as
// Evict.
func (r *Reconciler) Restore(ctx context.Context, path string) error {
	entry, ok, err := r.Store.EntryLookup(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("restore: no row for %q", path)
	}
	if entry.ArchiveState != ArchiveStateUnpinned {
		return fmt.Errorf("restore: %q is in state %q; need 'unpinned'", path, entry.ArchiveState)
	}

	fullPath := filepath.Join(r.HubDir, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	// Stage the download in .hubsync/tmp/ so the scanner's ignorer swallows
	// the intermediate file (otherwise fsnotify fires on create, FullScan
	// would materialize a row, and we'd race with the rename).
	stageDir := filepath.Join(r.HubDir, ".hubsync", "tmp")
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return fmt.Errorf("mkdir stage: %w", err)
	}
	tmp, err := os.CreateTemp(stageDir, "restore-*")
	if err != nil {
		return fmt.Errorf("tmpfile: %w", err)
	}
	cleanupTmp := true
	defer func() {
		tmp.Close()
		if cleanupTmp {
			os.Remove(tmp.Name())
		}
	}()

	h := r.Hasher.New()
	mw := io.MultiWriter(tmp, h)
	if err := r.Storage.Download(ctx, archive.JoinKey(r.Prefix, path), mw); err != nil {
		return err
	}
	gotDigest := Digest(h.Sum(nil))
	if gotDigest != entry.Digest {
		return fmt.Errorf("restore %s: digest mismatch (got %s want %s)", path, gotDigest.Hex(), entry.Digest.Hex())
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if entry.Mode != 0 {
		_ = os.Chmod(tmp.Name(), os.FileMode(entry.Mode))
	}

	if err := r.Store.MarkArchived(path, entry.ArchiveFileID, entry.ArchiveSHA1.Bytes(), entry.ArchiveUploadedAt); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), fullPath); err != nil {
		return fmt.Errorf("rename %s: %w", fullPath, err)
	}
	cleanupTmp = false
	return nil
}

// seedRow inserts a hub_entry row for a local file that hasn't been scanned
// yet. Callers use it during pin/unpin of a brand-new path.
func (r *Reconciler) seedRow(path string) error {
	fullPath := filepath.Join(r.HubDir, path)
	st, err := os.Stat(fullPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return err
	}
	digest := r.Hasher.Sum(data)
	_, err = r.Store.Append(ChangeEntry{
		Path:   path,
		Op:     OpCreate,
		Kind:   FileKindFile,
		Digest: digest,
		Size:   st.Size(),
		Mode:   uint32(st.Mode().Perm()),
		MTime:  st.ModTime().Unix(),
	})
	return err
}

