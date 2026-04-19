package taskrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

// archiveFilename matches "<YYYY-MM-DDTHHMMSSZ>-<basename>" — the
// timestamp prefix this package writes when archiving on successful
// drain. Kept narrow so a stray stamp format change surfaces loudly.
var archiveFilename = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{6}Z-archive\.sqlite$`)

func newEchoRunner(t *testing.T, dbPath string, plan func(emit func(*echoTask))) (*Runner[*echoTask], *[]string, *sync.Mutex) {
	t.Helper()
	var ran []string
	var ranMu sync.Mutex
	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			plan(emit)
			return nil
		},
		Decode: decoderFor(&ran, &ranMu, nil, nil),
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return r, &ran, &ranMu
}

// TestArchive_DefaultMovesOnSuccess is the happy path: Execute drains
// cleanly, the DB file is renamed with the UTC timestamp prefix, the
// original path is gone, and the WAL sidecars are not left behind.
func TestArchive_DefaultMovesOnSuccess(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	r, _, _ := newEchoRunner(t, dbPath, func(emit func(*echoTask)) {
		emit(&echoTask{ID: "a"})
		emit(&echoTask{ID: "b"})
	})
	defer r.Close()

	before := time.Now().UTC().Add(-time.Second)
	if err := r.Execute(context.Background(), RunOptions{Workers: 2}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	archived := r.ArchivedPath()
	if archived == "" {
		t.Fatal("ArchivedPath empty; expected a rename")
	}

	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original DB still present: err=%v", err)
	}
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("archived path missing: %v", err)
	}

	base := filepath.Base(archived)
	if !archiveFilename.MatchString(base) {
		t.Fatalf("archived basename %q does not match timestamp pattern", base)
	}
	// Timestamp in the filename should fall in [before, after].
	stamp := base[:len("2006-01-02T150405Z")]
	got, err := time.Parse("2006-01-02T150405Z", stamp)
	if err != nil {
		t.Fatalf("parse stamp %q: %v", stamp, err)
	}
	if got.Before(before) || got.After(after) {
		t.Errorf("stamp %v out of bounds [%v, %v]", got, before, after)
	}

	// Sidecars must be gone from the canonical path so a fresh
	// invocation doesn't trip over stragglers.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("leftover sidecar %s%s: %v", dbPath, suffix, err)
		}
	}

	// Summary must reflect the drained state for callers that already
	// closed the DB via archive.
	s := r.Summary()
	if s.Done != 2 || s.Pending != 0 || s.Running != 0 || s.Failed != 0 {
		t.Errorf("unexpected summary: %+v", s)
	}
}

// TestArchive_KeepDBOptOut: --keep-db preserves same-path resume
// semantics — the DB stays put.
func TestArchive_KeepDBOptOut(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	r, _, _ := newEchoRunner(t, dbPath, func(emit func(*echoTask)) {
		emit(&echoTask{ID: "a"})
	})
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{Workers: 1, KeepDB: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p := r.ArchivedPath(); p != "" {
		t.Errorf("ArchivedPath=%q with KeepDB=true; want empty", p)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("DB missing at original path with KeepDB=true: %v", err)
	}
}

// TestArchive_FailedRowsStay: with tasks remaining in 'failed'
// (retries exhausted), archive MUST NOT move the DB — the file's
// value is that a resume from the same path is meaningful.
func TestArchive_FailedRowsStay(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	// alwaysFail: task that unconditionally errors.
	cfg := Config[*alwaysFail]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*alwaysFail)) error {
			emit(&alwaysFail{ID: "bad"})
			return nil
		},
		Decode: func(row map[string]any) (*alwaysFail, error) {
			id, _ := row["id"].(string)
			return &alwaysFail{ID: id}, nil
		},
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{Workers: 1, MaxAttempts: 2}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if p := r.ArchivedPath(); p != "" {
		t.Errorf("ArchivedPath=%q but failed rows remain; should not archive", p)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("DB missing at original path despite failed rows: %v", err)
	}
	if s := r.Summary(); s.Failed == 0 {
		t.Errorf("expected at least one failed row; summary=%+v", s)
	}
}

// TestArchive_InMemorySkips: in-memory runners have no file to move;
// archive must be a silent no-op, not an error.
func TestArchive_InMemorySkips(t *testing.T) {
	r, _, _ := newEchoRunner(t, "", func(emit func(*echoTask)) {
		emit(&echoTask{ID: "a"})
	})
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{Workers: 1}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p := r.ArchivedPath(); p != "" {
		t.Errorf("ArchivedPath=%q with in-memory DB; want empty", p)
	}
}

// TestArchive_DrainedDBGoneNextPlanRuns: the whole point of the feature
// — archiving leaves the canonical path clean, and a subsequent Runner
// opened against the same path plans fresh and drains again.
func TestArchive_DrainedDBGoneNextPlanRuns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	r1, ran1, _ := newEchoRunner(t, dbPath, func(emit func(*echoTask)) {
		emit(&echoTask{ID: "a"})
	})
	if err := r1.Execute(context.Background(), RunOptions{Workers: 1}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	if len(*ran1) != 1 {
		t.Fatalf("first run: expected 1 execution, got %d", len(*ran1))
	}
	if r1.ArchivedPath() == "" {
		t.Fatalf("first run did not archive")
	}
	_ = r1.Close()

	// Second runner at the same canonical path — DB must be absent,
	// Plan runs again, task executes again.
	r2, ran2, _ := newEchoRunner(t, dbPath, func(emit func(*echoTask)) {
		emit(&echoTask{ID: "a"})
	})
	defer r2.Close()
	if err := r2.Execute(context.Background(), RunOptions{Workers: 1}); err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if len(*ran2) != 1 {
		t.Fatalf("second run: expected fresh execution, got %d", len(*ran2))
	}
}

// TestArchive_CloseIsIdempotent: archive-on-success closes the DB
// internally; the caller's `defer r.Close()` must not error.
func TestArchive_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	r, _, _ := newEchoRunner(t, dbPath, func(emit func(*echoTask)) {
		emit(&echoTask{ID: "a"})
	})
	if err := r.Execute(context.Background(), RunOptions{Workers: 1}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// First Close inside archiveIfDrained already ran; this one must be a no-op.
	if err := r.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}
}

// TestArchive_DestPathComposition pins the filename format to the
// documented layout so a stray time.Format change surfaces loudly.
func TestArchive_DestPathComposition(t *testing.T) {
	src := "/tmp/x/archive.sqlite"
	stamp := time.Date(2026, 4, 19, 14, 35, 22, 0, time.UTC)
	got := archiveDestPath(src, stamp)
	want := "/tmp/x/2026-04-19T143522Z-archive.sqlite"
	if got != want {
		t.Fatalf("archiveDestPath:\n got=%s\nwant=%s", got, want)
	}
}

// alwaysFail is a Task that unconditionally errors. Used to verify
// the failed-rows-stay invariant.
type alwaysFail struct {
	ID string `json:"id"`
}

func (a *alwaysFail) Run(ctx context.Context) (any, error) {
	return nil, fmt.Errorf("boom")
}
