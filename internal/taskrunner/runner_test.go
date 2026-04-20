package taskrunner

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// echoTask is a minimal Task used by the runner tests. Each Run appends
// its ID to a shared slice guarded by a mutex; failure / status are
// toggled via fields set at Hydrate time.
type echoTask struct {
	ID   string `json:"id"`
	Note string `json:"note"`

	// transient — filled by Hydrate, not in items JSON
	ran      *[]string
	ranMu    *sync.Mutex
	failOnce map[string]bool
	failed   *sync.Map // id → bool (has already failed once)
	sleep    time.Duration
}

func (t *echoTask) Run(ctx context.Context) (any, error) {
	if t.sleep > 0 {
		select {
		case <-time.After(t.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	t.ranMu.Lock()
	*t.ran = append(*t.ran, t.ID)
	t.ranMu.Unlock()
	if t.failOnce != nil && t.failOnce[t.ID] {
		if _, already := t.failed.LoadOrStore(t.ID, true); !already {
			return nil, fmt.Errorf("fail-once: %s", t.ID)
		}
	}
	return map[string]any{"note": t.Note}, nil
}

// echoFactory implements TaskFactory for the echo tests. The plan is
// provided as a closure; Hydrate returns an echoTask with the shared
// test-state pointers injected.
type echoFactory struct {
	plan     func(yield func(Task, error) bool)
	ran      *[]string
	ranMu    *sync.Mutex
	failOnce map[string]bool
	failed   *sync.Map
	sleep    time.Duration
	// hydrateCount is bumped on every Hydrate call so tests can verify
	// per-claim allocation.
	hydrateCount *int64
}

func (f *echoFactory) List(ctx context.Context) iter.Seq2[Task, error] {
	return f.plan
}

func (f *echoFactory) Hydrate(ctx context.Context) (Task, error) {
	if f.hydrateCount != nil {
		atomic.AddInt64(f.hydrateCount, 1)
	}
	return &echoTask{
		ran:      f.ran,
		ranMu:    f.ranMu,
		failOnce: f.failOnce,
		failed:   f.failed,
		sleep:    f.sleep,
	}, nil
}

// planOf wraps a plain "emit tasks" closure into the iter.Seq2 shape
// the factory needs, so each test can write its emit loop tersely.
func planOf(emit func(yield func(Task) bool)) func(yield func(Task, error) bool) {
	return func(yield func(Task, error) bool) {
		emit(func(t Task) bool {
			return yield(t, nil)
		})
	}
}

// newEchoFactory constructs an echoFactory over the given emit closure
// and shared state pointers.
func newEchoFactory(
	ran *[]string,
	ranMu *sync.Mutex,
	failOnce map[string]bool,
	failed *sync.Map,
	emit func(yield func(Task) bool),
) *echoFactory {
	return &echoFactory{
		plan:     planOf(emit),
		ran:      ran,
		ranMu:    ranMu,
		failOnce: failOnce,
		failed:   failed,
	}
}

// runnerForEcho constructs a Runner around an echoFactory. Hides the
// ceremony of wiring shared state + opts.
func runnerForEcho(t *testing.T, dbPath string, f *echoFactory) *Runner {
	t.Helper()
	r, err := New(context.Background(), Config{Factory: f}, RunOptions{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExecute_PlanAndDrain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "run.duckdb")

	var ran []string
	var ranMu sync.Mutex
	f := newEchoFactory(&ran, &ranMu, nil, nil, func(yield func(Task) bool) {
		for _, pair := range []struct{ id, note string }{{"a", "one"}, {"b", "two"}, {"c", "three"}} {
			if !yield(&echoTask{ID: pair.id, Note: pair.note}) {
				return
			}
		}
	})
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	ctx := context.Background()
	// KeepDB: archive-on-success would close the handle, breaking the
	// second-Execute-on-same-runner assertion below.
	if err := r.Execute(ctx, RunOptions{Workers: 2, HeartbeatEvery: 50 * time.Millisecond, KeepDB: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	ranMu.Lock()
	got := append([]string(nil), ran...)
	ranMu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected 3 runs, got %d: %v", len(got), got)
	}

	// Second Execute on the same DB: everything is done, no-op.
	if err := r.Execute(ctx, RunOptions{Workers: 2, KeepDB: true}); err != nil {
		t.Fatalf("execute (second): %v", err)
	}
	ranMu.Lock()
	if len(ran) != 3 {
		t.Fatalf("expected no new runs on second pass, got %d: %v", len(ran), ran)
	}
	ranMu.Unlock()

	// Sanity-check state via raw SQL.
	var done, pending, failed int
	if err := r.DB().QueryRow(
		`SELECT
		   SUM(CASE WHEN status='done' THEN 1 ELSE 0 END),
		   SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),
		   SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END)
		 FROM tasks`).Scan(&done, &pending, &failed); err != nil {
		t.Fatal(err)
	}
	if done != 3 || pending != 0 || failed != 0 {
		t.Fatalf("done=%d pending=%d failed=%d", done, pending, failed)
	}
}

func TestExecute_DryIsPlanOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dry.duckdb")
	var ran []string
	var ranMu sync.Mutex
	f := newEchoFactory(&ran, &ranMu, nil, nil, func(yield func(Task) bool) {
		for _, id := range []string{"x", "y"} {
			if !yield(&echoTask{ID: id}) {
				return
			}
		}
	})
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{Dry: true}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Fatalf("dry should not run tasks, got %v", ran)
	}
	var n int
	if err := r.DB().QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("items populated but count=%d", n)
	}
}

func TestExecute_WhereSubset(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "where.duckdb")
	var ran []string
	var ranMu sync.Mutex
	f := newEchoFactory(&ran, &ranMu, nil, nil, func(yield func(Task) bool) {
		for _, id := range []string{"a", "b", "c"} {
			if !yield(&echoTask{ID: id, Note: id}) {
				return
			}
		}
	})
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	// Only "a".
	if err := r.Execute(context.Background(), RunOptions{
		Workers: 1,
		Where:   "id = 'a'",
	}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != "a" {
		t.Fatalf("where subset failed: %v", ran)
	}

	// Second invocation with no where: picks up b and c.
	if err := r.Execute(context.Background(), RunOptions{Workers: 2}); err != nil {
		t.Fatal(err)
	}
	ranMu.Lock()
	got := map[string]bool{}
	for _, id := range ran {
		got[id] = true
	}
	ranMu.Unlock()
	if !got["a"] || !got["b"] || !got["c"] {
		t.Fatalf("expected all three after both runs, got %v", ran)
	}
}

func TestExecute_WhereFalseNoOp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "false.duckdb")
	var ran []string
	var ranMu sync.Mutex
	f := newEchoFactory(&ran, &ranMu, nil, nil, func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
	})
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()
	if err := r.Execute(context.Background(), RunOptions{Where: "false"}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Fatalf("--where false ran tasks: %v", ran)
	}
}

func TestExecute_RetryOnFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retry.duckdb")
	var ran []string
	var ranMu sync.Mutex
	var failed sync.Map
	failOnce := map[string]bool{"a": true}

	f := newEchoFactory(&ran, &ranMu, failOnce, &failed, func(yield func(Task) bool) {
		yield(&echoTask{ID: "a"})
		yield(&echoTask{ID: "b"})
	})
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{
		Workers:     1,
		MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}

	// "a" should have run twice (one fail, one success).
	var aRuns int
	for _, id := range ran {
		if id == "a" {
			aRuns++
		}
	}
	if aRuns != 2 {
		t.Fatalf("a should have run 2 times (fail-once + retry), got %d; ran=%v", aRuns, ran)
	}

	if done := r.Summary().Done; done != 2 {
		t.Fatalf("expected 2 done, got %d", done)
	}
}

func TestExecute_StaleRunningReclaim(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stale.duckdb")
	var ran []string
	var ranMu sync.Mutex

	f := newEchoFactory(&ran, &ranMu, nil, nil, func(yield func(Task) bool) {
		yield(&echoTask{ID: "z"})
	})
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	// Plan only (dry so "z" doesn't run), then manually mark it as stale-running.
	if err := r.Execute(context.Background(), RunOptions{Dry: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec(
		`UPDATE tasks
		    SET status='running',
		        started_at   = datetime('now', '-1 hour'),
		        heartbeat_at = datetime('now', '-1 hour'),
		        attempts     = 1
		  WHERE id='z'`); err != nil {
		t.Fatal(err)
	}

	// Execute with a short stale threshold; the row should be reclaimed.
	if err := r.Execute(context.Background(), RunOptions{
		Workers: 1,
		Stale:   5 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	ranMu.Lock()
	defer ranMu.Unlock()
	if len(ran) != 1 || ran[0] != "z" {
		t.Fatalf("stale-reclaim did not re-run: %v", ran)
	}
}

func TestExecute_ConcurrentWorkers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "conc.duckdb")
	var ran []string
	var ranMu sync.Mutex
	var counter int64

	f := &echoFactory{
		plan: planOf(func(yield func(Task) bool) {
			for i := 0; i < 20; i++ {
				if !yield(&echoTask{ID: fmt.Sprintf("t%02d", i)}) {
					return
				}
			}
		}),
		ran:          &ran,
		ranMu:        &ranMu,
		sleep:        10 * time.Millisecond,
		hydrateCount: &counter,
	}
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 20 {
		t.Fatalf("expected 20 runs, got %d", len(ran))
	}
	// Hydrate fires once per claim + once at New (schema-inference probe).
	if got := atomic.LoadInt64(&counter); got != 21 {
		t.Fatalf("Hydrate should fire 21x (1 probe + 20 claims), got %d", got)
	}
}

// TestExecute_FeedBackpressureDoesNotDeadlock is the regression test
// for the bug where feedQueue pinned the single DuckDB connection via
// the rows iterator while pushing into a bounded channel.
func TestExecute_FeedBackpressureDoesNotDeadlock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backpressure.duckdb")

	const N = 40
	var ran []string
	var ranMu sync.Mutex

	f := &echoFactory{
		plan: planOf(func(yield func(Task) bool) {
			for i := 0; i < N; i++ {
				if !yield(&echoTask{ID: fmt.Sprintf("t%03d", i)}) {
					return
				}
			}
		}),
		ran:   &ran,
		ranMu: &ranMu,
		sleep: 2 * time.Millisecond,
	}
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	// 2 workers → channel buffer = 4 → in-flight capacity = 6.
	// With N=40, feedQueue MUST release the conn before blocking on
	// the channel, or the claim path starves.
	done := make(chan error, 1)
	go func() {
		done <- r.Execute(context.Background(), RunOptions{Workers: 2})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute deadlocked: did not finish within 10s for N=40 rows, 2 workers")
	}

	ranMu.Lock()
	defer ranMu.Unlock()
	if len(ran) != N {
		t.Fatalf("expected %d runs, got %d", N, len(ran))
	}
}

// TestExecute_ConcurrentReaderSeesProgress confirms WAL allows a
// second *sql.DB handle to see task state mid-run.
func TestExecute_ConcurrentReaderSeesProgress(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concread.duckdb")

	const N = 12
	var ran []string
	var ranMu sync.Mutex

	f := &echoFactory{
		plan: planOf(func(yield func(Task) bool) {
			for i := 0; i < N; i++ {
				if !yield(&echoTask{ID: fmt.Sprintf("t%02d", i)}) {
					return
				}
			}
		}),
		ran:   &ran,
		ranMu: &ranMu,
		sleep: 50 * time.Millisecond,
	}
	r := runnerForEcho(t, dbPath, f)
	defer r.Close()

	execDone := make(chan error, 1)
	go func() {
		execDone <- r.Execute(context.Background(), RunOptions{Workers: 2})
	}()

	readerDB, err := sql.Open("sqlite", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer readerDB.Close()

	deadline := time.Now().Add(5 * time.Second)
	var sawDone bool
	for time.Now().Before(deadline) {
		var done int
		err := readerDB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status='done'`).Scan(&done)
		if err != nil {
			t.Fatalf("reader query: %v", err)
		}
		if done > 0 {
			sawDone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !sawDone {
		t.Fatal("second handle never saw a 'done' task while writer was running (WAL concurrency broken?)")
	}

	if err := <-execDone; err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(ran) != N {
		t.Fatalf("expected %d runs, got %d", N, len(ran))
	}
}
