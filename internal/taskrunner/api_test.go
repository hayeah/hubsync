package taskrunner

import (
	"context"
	"errors"
	"flag"
	"iter"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// TestRun_BindFlagsAndExitCodesClean exercises the packaged Run path
// with a clean-drain factory: BindFlags parses --resume/--workers,
// Run returns nil, RunResult reflects the summary.
func TestRun_BindFlagsAndExitCodesClean(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bindflags-clean.sqlite")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := BindFlags(fs)
	if err := fs.Parse([]string{"--resume", dbPath, "--workers", "1"}); err != nil {
		t.Fatal(err)
	}
	if opts.DBPath != dbPath {
		t.Fatalf("--resume did not bind to DBPath: %q", opts.DBPath)
	}
	if opts.Workers != 1 {
		t.Fatalf("--workers did not bind: %d", opts.Workers)
	}

	var ran []string
	var ranMu sync.Mutex
	f := newEchoFactory(&ran, &ranMu, nil, nil, func(yield func(Task) bool) {
		yield(&echoTask{ID: "clean-a"})
		yield(&echoTask{ID: "clean-b"})
	})

	result, err := Run(context.Background(), Config{Factory: f}, *opts)
	if err != nil {
		t.Fatalf("Run returned %v; want nil on clean drain", err)
	}
	if result.Summary.Done != 2 {
		t.Errorf("Summary.Done=%d want 2", result.Summary.Done)
	}
	if result.Summary.Failed != 0 {
		t.Errorf("Summary.Failed=%d want 0", result.Summary.Failed)
	}
	if result.ArchivedPath == "" {
		t.Errorf("clean drain should have archived; ArchivedPath empty")
	}
}

// TestRun_ErrFailedRemain exercises the fail-branch: a factory whose
// every task errors should return ErrFailedRemain wrapped from Run,
// with a populated RunResult the caller can still log.
func TestRun_ErrFailedRemain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bindflags-fail.sqlite")

	opts := RunOptions{DBPath: dbPath, Workers: 1, MaxAttempts: 1, KeepDB: true}

	result, err := Run(context.Background(), Config{Factory: &alwaysFailFactory{}}, opts)
	if !errors.Is(err, ErrFailedRemain) {
		t.Fatalf("Run err=%v; want errors.Is(ErrFailedRemain)", err)
	}
	if result.Summary.Failed != 1 {
		t.Errorf("Summary.Failed=%d want 1", result.Summary.Failed)
	}
	if result.Summary.Done != 0 {
		t.Errorf("Summary.Done=%d want 0", result.Summary.Done)
	}
}

// TestRun_ErrSetup wraps factory-less Config to hit the setup path.
// Run must return an error that errors.Is(ErrSetup) so Main can map to
// exit code 2.
func TestRun_ErrSetup(t *testing.T) {
	_, err := Run(context.Background(), Config{Factory: nil}, RunOptions{})
	if !errors.Is(err, ErrSetup) {
		t.Fatalf("Run err=%v; want errors.Is(ErrSetup)", err)
	}
}

// jsonCols is a Task with a JSON-typed column ([]string). Today's echo
// corpus has no isJSON fields so the round-trip path went untested.
// This type exercises the runner's json.RawMessage substitution in
// hydrate().
type jsonCols struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	Meta jsonMeta `json:"meta"`

	// shared test state
	got chan<- jsonCols `json:"-"`
}

type jsonMeta struct {
	Priority int      `json:"priority"`
	Labels   []string `json:"labels"`
}

func (t *jsonCols) Run(ctx context.Context) (any, error) {
	// Snapshot the fields the runner unmarshaled so the test can assert
	// on the round-trip without racing on the concrete Task pointer.
	t.got <- jsonCols{ID: t.ID, Tags: append([]string(nil), t.Tags...), Meta: t.Meta}
	return nil, nil
}

type jsonColsFactory struct {
	emit []jsonCols
	got  chan jsonCols
}

func (f *jsonColsFactory) List(ctx context.Context) iter.Seq2[Task, error] {
	return func(yield func(Task, error) bool) {
		for _, v := range f.emit {
			t := v
			if !yield(&t, nil) {
				return
			}
		}
	}
}
func (f *jsonColsFactory) Hydrate(ctx context.Context) (Task, error) {
	return &jsonCols{got: f.got}, nil
}

// TestHydrate_JSONColumn plans a row with a slice + nested-struct
// field, forces a resume (second runner on the same DB after KeepDB
// plan-only), and asserts the re-hydrated Task's JSON fields survive
// byte-for-byte. Guards the json.RawMessage substitution in hydrate().
func TestHydrate_JSONColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jsoncol.sqlite")

	want := jsonCols{
		ID:   "row-1",
		Tags: []string{"alpha", "beta", "gamma"},
		Meta: jsonMeta{Priority: 42, Labels: []string{"x", "y"}},
	}

	// First runner — plan only, items populated, keep DB for second pass.
	got := make(chan jsonCols, 1)
	f1 := &jsonColsFactory{emit: []jsonCols{want}, got: got}
	r1, err := New(context.Background(), Config{Factory: f1}, RunOptions{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Execute(context.Background(), RunOptions{Dry: true}); err != nil {
		t.Fatalf("plan-only execute: %v", err)
	}
	_ = r1.Close()

	// Second runner — items already present; Execute goes straight to drain.
	// This is the code path that hits hydrate() with the persisted row.
	f2 := &jsonColsFactory{emit: nil, got: got}
	r2, err := New(context.Background(), Config{Factory: f2}, RunOptions{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if err := r2.Execute(context.Background(), RunOptions{Workers: 1, KeepDB: true}); err != nil {
		t.Fatalf("drain execute: %v", err)
	}

	select {
	case g := <-got:
		if g.ID != want.ID {
			t.Errorf("ID=%q want %q", g.ID, want.ID)
		}
		if !reflect.DeepEqual(g.Tags, want.Tags) {
			t.Errorf("Tags mismatch: got %v want %v", g.Tags, want.Tags)
		}
		if !reflect.DeepEqual(g.Meta, want.Meta) {
			t.Errorf("Meta mismatch: got %+v want %+v", g.Meta, want.Meta)
		}
	default:
		t.Fatal("Run never fired; re-hydrate did not reach the task")
	}
}

// ctxCancelFactory yields one task, then checks ctx.Err on the next
// iteration to emulate a user factory that honors cancellation mid-
// enumerate.
type ctxCancelFactory struct {
	cancel func()
}

func (f *ctxCancelFactory) List(ctx context.Context) iter.Seq2[Task, error] {
	return func(yield func(Task, error) bool) {
		// First task goes through fine.
		if !yield(&echoTask{ID: "first"}, nil) {
			return
		}
		// Now cancel; next iteration checks ctx.Err.
		f.cancel()
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		yield(&echoTask{ID: "second"}, nil)
	}
}
func (f *ctxCancelFactory) Hydrate(ctx context.Context) (Task, error) {
	return &echoTask{}, nil
}

// TestList_ContextCancellation verifies that a factory honoring
// ctx.Err() during List surfaces as a plan error. The runner must not
// swallow the cancellation.
func TestList_ContextCancellation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ctxcancel.sqlite")

	ctx, cancel := context.WithCancel(context.Background())
	f := &ctxCancelFactory{cancel: cancel}

	r, err := New(context.Background(), Config{Factory: f}, RunOptions{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	err = r.Execute(ctx, RunOptions{Dry: true})
	if err == nil {
		t.Fatal("Execute returned nil; expected context.Canceled via plan")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute err=%v; want errors.Is(context.Canceled)", err)
	}

	// The first task made it into items before cancel; the second did not.
	var n int
	if err := r.DB().QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err == nil {
		// Items-count assertion: we want to confirm the plan loop bailed
		// early. It's possible items was never created (error aborted
		// the tx); either 0 or 1 is acceptable, but 2 means the cancel
		// was ignored.
		if n == 2 {
			t.Errorf("plan loop ignored cancel; both rows landed (n=%d)", n)
		}
	}
}
