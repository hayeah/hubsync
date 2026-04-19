package taskrunner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
)

// Config wires up a domain type T to the runner. Plan is called exactly
// when items is empty (first invocation or after a manual wipe); Decode
// is called on every row the claim loop picks up.
type Config[T Task] struct {
	// DBPath is the DuckDB file. Empty uses ":memory:".
	DBPath string

	// Plan emits one T per unit of work. The runner serializes each to
	// JSON, writes a JSONL temp file, and materializes the items table
	// via DuckDB's read_json_auto. Every emitted row must carry a
	// stable, unique "id" field.
	Plan func(ctx context.Context, emit func(T)) error

	// Decode turns one items row into a runnable T. The row map contains
	// all columns (items joined with tasks — but the project code only
	// needs the items fields). The runner passes the same transient
	// dependencies the caller closed over in Plan; in practice both Plan
	// and Decode are constructed with the same *HubStore / storage
	// client pointer in scope.
	Decode func(row map[string]any) (T, error)
}

// RunOptions tunes a single Execute call.
type RunOptions struct {
	Workers        int           // default runtime.NumCPU()
	Where          string        // SQL fragment AND-combined with work-queue predicate
	MaxAttempts    int           // default 3
	Stale          time.Duration // default 30s
	HeartbeatEvery time.Duration // default 5s
	Dry            bool          // plan-if-empty, don't run
}

// Runner owns the DB handle and serialized-write mutex.
type Runner[T Task] struct {
	cfg Config[T]
	db  *sql.DB

	// writeMu serializes every writing statement against the single DuckDB
	// connection. DuckDB is process-single-writer and the go driver
	// happily hands out parallel conns from the pool; we avoid the
	// "database is locked" path by funneling writes ourselves.
	writeMu sync.Mutex

	// planErr captures the first error seen during the emit callback.
	planErr error
}

// New opens the DB and ensures the tasks/runs tables exist. The items
// table is created later, on first plan.
func New[T Task](cfg Config[T]) (*Runner[T], error) {
	if cfg.Plan == nil {
		return nil, errors.New("taskrunner: Config.Plan is required")
	}
	if cfg.Decode == nil {
		return nil, errors.New("taskrunner: Config.Decode is required")
	}
	dsn := cfg.DBPath
	if dsn == "" {
		dsn = ""
	}
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: open %s: %w", cfg.DBPath, err)
	}
	// DuckDB cannot serve concurrent writers on a single handle; restrict
	// the pool to one connection and let the writeMu serialize callers.
	db.SetMaxOpenConns(1)
	if err := ensureTasksTable(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Runner[T]{cfg: cfg, db: db}, nil
}

// Close releases the DB handle.
func (r *Runner[T]) Close() error { return r.db.Close() }

// Execute is the one-verb operation: plan-if-needed, select unfinished
// rows (+ user --where), and drain them through a worker pool.
func (r *Runner[T]) Execute(ctx context.Context, opts RunOptions) error {
	opts = opts.withDefaults()

	empty, err := itemsEmpty(r.db)
	if err != nil {
		return err
	}
	if empty {
		if err := r.runPlan(ctx); err != nil {
			return err
		}
	}

	if opts.Dry {
		return nil
	}

	return r.drain(ctx, opts)
}

func (o RunOptions) withDefaults() RunOptions {
	if o.Workers <= 0 {
		o.Workers = runtime.NumCPU()
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.Stale <= 0 {
		o.Stale = 30 * time.Second
	}
	if o.HeartbeatEvery <= 0 {
		o.HeartbeatEvery = 5 * time.Second
	}
	return o
}

// drain picks unfinished rows in batches, feeds them to the worker pool,
// and returns when the queue empties (or ctx cancels).
func (r *Runner[T]) drain(ctx context.Context, opts RunOptions) error {
	queue := make(chan map[string]any, opts.Workers*2)
	var wg sync.WaitGroup
	wg.Add(opts.Workers)

	var workerErr error
	var workerErrMu sync.Mutex
	recordErr := func(err error) {
		if err == nil {
			return
		}
		workerErrMu.Lock()
		defer workerErrMu.Unlock()
		if workerErr == nil {
			workerErr = err
		}
	}

	for i := 0; i < opts.Workers; i++ {
		go func() {
			defer wg.Done()
			for row := range queue {
				if err := r.processRow(ctx, row, opts); err != nil {
					recordErr(err)
				}
			}
		}()
	}

	err := r.feedQueue(ctx, opts, queue)
	close(queue)
	wg.Wait()

	if err != nil {
		return err
	}
	if workerErr != nil {
		return workerErr
	}
	return nil
}

// feedQueue streams unfinished rows into the queue until the work queue
// is empty for a full pass. A "pass" is one SELECT; between passes we
// wait briefly so workers can surface new failures before we re-scan.
func (r *Runner[T]) feedQueue(ctx context.Context, opts RunOptions, queue chan<- map[string]any) error {
	where := r.workQueueWhere(opts)
	query := fmt.Sprintf(
		`SELECT * FROM items JOIN tasks USING (id) WHERE %s ORDER BY id`,
		where,
	)

	// One pass: emit every currently-eligible row. The claim UPDATE
	// inside processRow re-checks the lease; if another worker got there
	// first the row is skipped silently.
	for {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("taskrunner: select work queue: %w", err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return fmt.Errorf("taskrunner: columns: %w", err)
		}

		emitted := 0
		for rows.Next() {
			raw := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return fmt.Errorf("taskrunner: scan: %w", err)
			}
			row := make(map[string]any, len(cols))
			for i, name := range cols {
				row[name] = raw[i]
			}
			select {
			case queue <- row:
				emitted++
			case <-ctx.Done():
				rows.Close()
				return ctx.Err()
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("taskrunner: iterate: %w", err)
		}
		rows.Close()

		if emitted == 0 {
			return nil
		}
		// Let workers drain this batch before we re-scan for newly
		// freed stale-running rows / retries. This is a soft barrier —
		// we don't block; we just re-scan after a short delay.
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// workQueueWhere returns the unified WHERE fragment (built-in predicate
// AND user --where). Reserved state-column names in tasks prevent name
// collisions with items.
func (r *Runner[T]) workQueueWhere(opts RunOptions) string {
	builtin := fmt.Sprintf(
		`(status='pending'
		   OR (status='failed'  AND attempts<%d)
		   OR (status='running' AND (heartbeat_at IS NULL OR heartbeat_at < now() - INTERVAL %d SECOND)))`,
		opts.MaxAttempts,
		int(opts.Stale.Seconds()),
	)
	if strings.TrimSpace(opts.Where) == "" {
		return builtin
	}
	return fmt.Sprintf("%s AND (%s)", builtin, opts.Where)
}

// processRow claims one row, runs the task (with heartbeat), and writes
// the terminal status.
func (r *Runner[T]) processRow(ctx context.Context, row map[string]any, opts RunOptions) error {
	id, _ := row["id"].(string)
	if id == "" {
		return fmt.Errorf("taskrunner: row missing id")
	}

	claimed, err := r.claim(ctx, id, opts)
	if err != nil {
		return err
	}
	if !claimed {
		// Another worker / another process grabbed it first.
		return nil
	}

	task, decodeErr := r.cfg.Decode(row)
	if decodeErr != nil {
		return r.writeTerminal(ctx, id, "failed", fmt.Errorf("decode: %w", decodeErr), nil)
	}

	// Heartbeat goroutine lives for the duration of Run.
	hbCtx, stopHB := context.WithCancel(ctx)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		r.heartbeatLoop(hbCtx, id, task, opts.HeartbeatEvery)
	}()

	result, runErr := task.Run(ctx)

	stopHB()
	hbWG.Wait()

	if runErr != nil {
		return r.writeTerminal(ctx, id, "failed", runErr, nil)
	}
	return r.writeTerminal(ctx, id, "done", nil, result)
}

// claim atomically flips an eligible row to 'running', bumping attempts.
// Returns true if the UPDATE touched a row (we own the lease).
func (r *Runner[T]) claim(ctx context.Context, id string, opts RunOptions) (bool, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	q := fmt.Sprintf(
		`UPDATE tasks
		    SET status       = 'running',
		        attempts     = attempts + 1,
		        started_at   = now(),
		        heartbeat_at = now(),
		        error        = NULL
		  WHERE id = ?
		    AND (status = 'pending'
		      OR (status = 'failed'  AND attempts < %d)
		      OR (status = 'running' AND (heartbeat_at IS NULL
		          OR heartbeat_at < now() - INTERVAL %d SECOND)))`,
		opts.MaxAttempts,
		int(opts.Stale.Seconds()),
	)
	res, err := r.db.ExecContext(ctx, q, id)
	if err != nil {
		return false, fmt.Errorf("taskrunner: claim %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("taskrunner: claim rowsaffected: %w", err)
	}
	return n > 0, nil
}

// heartbeatLoop refreshes heartbeat_at on a ticker. If T implements
// StatusReporter, the snapshot is merged into meta.status.
func (r *Runner[T]) heartbeatLoop(ctx context.Context, id string, task T, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	reporter, hasStatus := any(task).(StatusReporter)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var status any
			if hasStatus {
				s, err := reporter.Status()
				if err == nil {
					status = s
				}
			}
			r.beat(context.Background(), id, status)
		}
	}
}

func (r *Runner[T]) beat(ctx context.Context, id string, status any) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if status == nil {
		_, _ = r.db.ExecContext(ctx,
			`UPDATE tasks SET heartbeat_at = now() WHERE id = ?`, id)
		return
	}
	buf, err := json.Marshal(status)
	if err != nil {
		return
	}
	_, _ = r.db.ExecContext(ctx,
		`UPDATE tasks
		    SET heartbeat_at = now(),
		        meta = json_merge_patch(meta, json_object('status', ?::JSON))
		  WHERE id = ?`,
		string(buf), id)
}

// writeTerminal commits 'done' or 'failed' for a task and the optional
// result / error string.
func (r *Runner[T]) writeTerminal(ctx context.Context, id, status string, runErr error, result any) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if status == "done" {
		var resultJSON string
		if result != nil {
			buf, err := json.Marshal(result)
			if err != nil {
				// Record the marshal error as a failure instead of claiming success.
				status = "failed"
				runErr = fmt.Errorf("marshal result: %w", err)
			} else {
				resultJSON = string(buf)
			}
		}
		if status == "done" {
			_, err := r.db.ExecContext(ctx,
				`UPDATE tasks
				    SET status       = 'done',
				        ended_at     = now(),
				        heartbeat_at = now(),
				        error        = NULL,
				        meta = CASE WHEN ?::VARCHAR IS NULL THEN meta
				                    ELSE json_merge_patch(meta, json_object('result', ?::JSON))
				               END
				  WHERE id = ?`,
				nullIfEmpty(resultJSON), nullIfEmpty(resultJSON), id)
			if err != nil {
				return fmt.Errorf("taskrunner: terminal-done %s: %w", id, err)
			}
			return nil
		}
	}

	// failed path
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE tasks
		    SET status       = 'failed',
		        ended_at     = now(),
		        heartbeat_at = now(),
		        error        = ?
		  WHERE id = ?`,
		msg, id)
	if err != nil {
		return fmt.Errorf("taskrunner: terminal-failed %s: %w", id, err)
	}
	return nil
}

// nullIfEmpty returns sql.NullString so DuckDB binds NULL for an empty
// Go string (needed for the CASE WHEN NULL branch above).
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DB exposes the underlying handle for callers that need ad-hoc queries
// (tests, introspection). Writes should still go through the runner's
// own mutex; callers doing raw writes must handle their own
// serialization.
func (r *Runner[T]) DB() *sql.DB { return r.db }
