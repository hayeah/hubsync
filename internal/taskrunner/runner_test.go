package taskrunner

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// echoTask is a minimal Task used by the runner tests. Each Run appends
// its ID to a shared slice guarded by a mutex; failure / status are
// toggled via fields set by the Decode callback.
type echoTask struct {
	ID   string `json:"id"`
	Note string `json:"note"`

	// transient — filled by Decode, not in items JSON
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

// decoderFor returns a Decode callback closed over shared test state.
func decoderFor(ran *[]string, ranMu *sync.Mutex, failOnce map[string]bool, failed *sync.Map) func(map[string]any) (*echoTask, error) {
	return func(row map[string]any) (*echoTask, error) {
		id, _ := row["id"].(string)
		note, _ := row["note"].(string)
		return &echoTask{
			ID:       id,
			Note:     note,
			ran:      ran,
			ranMu:    ranMu,
			failOnce: failOnce,
			failed:   failed,
		}, nil
	}
}

func TestExecute_PlanAndDrain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "run.duckdb")

	var ran []string
	var ranMu sync.Mutex
	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			emit(&echoTask{ID: "a", Note: "one"})
			emit(&echoTask{ID: "b", Note: "two"})
			emit(&echoTask{ID: "c", Note: "three"})
			return nil
		},
		Decode: decoderFor(&ran, &ranMu, nil, nil),
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ctx := context.Background()
	if err := r.Execute(ctx, RunOptions{Workers: 2, HeartbeatEvery: 50 * time.Millisecond}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	ranMu.Lock()
	got := append([]string(nil), ran...)
	ranMu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected 3 runs, got %d: %v", len(got), got)
	}

	// Second Execute on the same DB: everything is done, no-op.
	if err := r.Execute(ctx, RunOptions{Workers: 2}); err != nil {
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
	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			for _, id := range []string{"x", "y"} {
				emit(&echoTask{ID: id})
			}
			return nil
		},
		Decode: decoderFor(&ran, &ranMu, nil, nil),
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
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
	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			for _, id := range []string{"a", "b", "c"} {
				emit(&echoTask{ID: id, Note: id})
			}
			return nil
		},
		Decode: decoderFor(&ran, &ranMu, nil, nil),
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
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
	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			emit(&echoTask{ID: "a"})
			return nil
		},
		Decode: decoderFor(&ran, &ranMu, nil, nil),
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
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

	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			emit(&echoTask{ID: "a"})
			emit(&echoTask{ID: "b"})
			return nil
		},
		Decode: decoderFor(&ran, &ranMu, failOnce, &failed),
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
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

	var done int
	if err := r.DB().QueryRow(`SELECT COUNT(*) FROM tasks WHERE status='done'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 2 {
		t.Fatalf("expected 2 done, got %d", done)
	}
}

func TestExecute_StaleRunningReclaim(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stale.duckdb")
	var ran []string
	var ranMu sync.Mutex

	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			emit(&echoTask{ID: "z"})
			return nil
		},
		Decode: decoderFor(&ran, &ranMu, nil, nil),
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Plan only (dry so "z" doesn't run), then manually mark it as stale-running.
	if err := r.Execute(context.Background(), RunOptions{Dry: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec(
		`UPDATE tasks
		    SET status='running', started_at=now() - INTERVAL 1 HOUR,
		        heartbeat_at=now() - INTERVAL 1 HOUR, attempts=1
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

	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			for i := 0; i < 20; i++ {
				emit(&echoTask{ID: fmt.Sprintf("t%02d", i)})
			}
			return nil
		},
		Decode: func(row map[string]any) (*echoTask, error) {
			id, _ := row["id"].(string)
			t := &echoTask{
				ID:    id,
				ran:   &ran,
				ranMu: &ranMu,
				sleep: 10 * time.Millisecond,
			}
			atomic.AddInt64(&counter, 1)
			return t, nil
		},
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Execute(context.Background(), RunOptions{Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 20 {
		t.Fatalf("expected 20 runs, got %d", len(ran))
	}
	if atomic.LoadInt64(&counter) != 20 {
		t.Fatalf("decoder not invoked once per row: %d", counter)
	}
}

// TestExecute_FeedBackpressureDoesNotDeadlock is the regression test
// for the bug where feedQueue pinned the single DuckDB connection via
// the rows iterator while pushing into a bounded channel — the first
// claim UPDATE blocked on the conn, the channel back-pressured, and
// the whole pipeline hung. With N rows > workers*(buffer+1), the
// deadlock reliably triggers against the pre-fix feedQueue.
func TestExecute_FeedBackpressureDoesNotDeadlock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backpressure.duckdb")

	const N = 40
	var ran []string
	var ranMu sync.Mutex

	cfg := Config[*echoTask]{
		DBPath: dbPath,
		Plan: func(ctx context.Context, emit func(*echoTask)) error {
			for i := 0; i < N; i++ {
				emit(&echoTask{ID: fmt.Sprintf("t%03d", i)})
			}
			return nil
		},
		Decode: func(row map[string]any) (*echoTask, error) {
			id, _ := row["id"].(string)
			return &echoTask{
				ID:    id,
				ran:   &ran,
				ranMu: &ranMu,
				sleep: 2 * time.Millisecond,
			}, nil
		},
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 2 workers → channel buffer = 4 → in-flight capacity = 6.
	// With N=40, feedQueue MUST release the DuckDB conn before
	// blocking on the channel, or the claim path starves.
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
