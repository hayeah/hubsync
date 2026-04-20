package taskrunner

import (
	"context"
	"errors"
	"fmt"
	"iter"
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

// newArchiveEchoRunner wires an echoFactory + Runner for archive-flow
// tests. Returns the runner plus the ran/ranMu pair for assertion.
func newArchiveEchoRunner(t *testing.T, dbPath string, emit func(yield func(Task) bool)) (*Runner, *[]string, *sync.Mutex) {
	t.Helper()
	var ran []string
	var ranMu sync.Mutex
	f := newEchoFactory(&ran, &ranMu, nil, nil, emit)
	r, err := New(context.Background(), Config{Factory: f}, RunOptions{DBPath: dbPath})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return r, &ran, &ranMu
}

// TestArchive_DefaultMovesOnSuccess is the happy path.
func TestArchive_DefaultMovesOnSuccess(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	r, _, _ := newArchiveEchoRunner(t, dbPath, func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
		yield(&echoTask{ID: "b"})
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
	stamp := base[:len("2006-01-02T150405Z")]
	got, err := time.Parse("2006-01-02T150405Z", stamp)
	if err != nil {
		t.Fatalf("parse stamp %q: %v", stamp, err)
	}
	if got.Before(before) || got.After(after) {
		t.Errorf("stamp %v out of bounds [%v, %v]", got, before, after)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("leftover sidecar %s%s: %v", dbPath, suffix, err)
		}
	}

	s := r.Summary()
	if s.Done != 2 || s.Pending != 0 || s.Running != 0 || s.Failed != 0 {
		t.Errorf("unexpected summary: %+v", s)
	}
}

// TestArchive_KeepDBOptOut: --keep-db preserves same-path resume semantics.
func TestArchive_KeepDBOptOut(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	r, _, _ := newArchiveEchoRunner(t, dbPath, func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
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
// (retries exhausted), archive MUST NOT move the DB.
func TestArchive_FailedRowsStay(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	f := &alwaysFailFactory{}
	r, err := New(context.Background(), Config{Factory: f}, RunOptions{DBPath: dbPath})
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

// TestArchive_InMemorySkips: in-memory runners have no file to move.
func TestArchive_InMemorySkips(t *testing.T) {
	r, _, _ := newArchiveEchoRunner(t, "", func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
	})
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{Workers: 1}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p := r.ArchivedPath(); p != "" {
		t.Errorf("ArchivedPath=%q with in-memory DB; want empty", p)
	}
}

// TestArchive_DrainedDBGoneNextPlanRuns: archiving leaves the canonical
// path clean, and a subsequent Runner against the same path plans fresh.
func TestArchive_DrainedDBGoneNextPlanRuns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.sqlite")

	r1, ran1, _ := newArchiveEchoRunner(t, dbPath, func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
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

	r2, ran2, _ := newArchiveEchoRunner(t, dbPath, func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
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

	r, _, _ := newArchiveEchoRunner(t, dbPath, func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
	})
	if err := r.Execute(context.Background(), RunOptions{Workers: 1}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}
}

// TestArchive_DestPathComposition pins the filename format.
func TestArchive_DestPathComposition(t *testing.T) {
	src := "/tmp/x/archive.sqlite"
	stamp := time.Date(2026, 4, 19, 14, 35, 22, 0, time.UTC)
	got := archiveDestPath(src, stamp)
	want := "/tmp/x/2026-04-19T143522Z-archive.sqlite"
	if got != want {
		t.Fatalf("archiveDestPath:\n got=%s\nwant=%s", got, want)
	}
}

// alwaysFail is a Task that unconditionally errors.
type alwaysFail struct {
	ID string `json:"id"`
}

func (a *alwaysFail) Run(ctx context.Context) (any, error) {
	return nil, fmt.Errorf("boom")
}

// alwaysFailFactory plans a single alwaysFail task.
type alwaysFailFactory struct{}

func (f *alwaysFailFactory) List(ctx context.Context) iter.Seq2[Task, error] {
	return func(yield func(Task, error) bool) {
		yield(&alwaysFail{ID: "bad"}, nil)
	}
}
func (f *alwaysFailFactory) Hydrate(ctx context.Context) (Task, error) {
	return &alwaysFail{}, nil
}
