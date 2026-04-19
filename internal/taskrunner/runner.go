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

	_ "modernc.org/sqlite"
)

// Config wires up a domain type T to the runner. Plan is called exactly
// when items is empty (first invocation or after a manual wipe); Decode
// is called on every row the claim loop picks up.
type Config[T Task] struct {
	// DBPath is the SQLite file path. Empty uses an in-memory database.
	// The file is opened in WAL journal mode so external readers (e.g.
	// `sqlite3 file.db "select … from tasks"`) can observe task state
	// while a writer is running.
	DBPath string

	// Plan emits one T per unit of work. The runner reflects on T's
	// struct tags to build a typed items table (with columns derived
	// from `json:"…"` tags) and streams each emitted row through a
	// prepared INSERT. Every emitted row must carry a non-empty `id`
	// field (the `id` JSON tag); id becomes items' PRIMARY KEY.
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

	// writeMu serializes writing statements within this process. SQLite in
	// WAL mode allows concurrent readers with a single writer; busy_timeout
	// absorbs cross-process contention, and writeMu spares us from that
	// retry loop for intra-process writes (claim / heartbeat / terminal).
	writeMu sync.Mutex

	// planErr captures the first error seen during the emit callback.
	planErr error
}

// New opens the SQLite database in WAL mode and ensures the tasks/runs
// tables exist. The items table is created lazily on first plan.
func New[T Task](cfg Config[T]) (*Runner[T], error) {
	if cfg.Plan == nil {
		return nil, errors.New("taskrunner: Config.Plan is required")
	}
	if cfg.Decode == nil {
		return nil, errors.New("taskrunner: Config.Decode is required")
	}
	dsn := sqliteDSN(cfg.DBPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: open %s: %w", cfg.DBPath, err)
	}
	if err := ensureTasksTable(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Runner[T]{cfg: cfg, db: db}, nil
}

// sqliteDSN builds a modernc.org/sqlite DSN that enables WAL, a 5s
// busy_timeout (so concurrent writers backoff instead of erroring), and
// foreign_keys (harmless here, sane default). An empty path maps to an
// in-memory DB that is also shared-cache so multiple *sql.DB handles
// from the same process can see the same data during tests.
func sqliteDSN(path string) string {
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	if path == "" {
		return "file::memory:?cache=shared&" + pragmas
	}
	return "file:" + path + "?" + pragmas
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

// feedQueue re-scans the work queue in passes until no new row is
// eligible. Dedup is by (id, attempts): we only emit a row again once
// its attempts counter has bumped (i.e. a previous attempt completed
// and terminal-wrote 'failed'). This both prevents double-queuing
// while workers are still running an attempt AND allows in-Execute
// retries for failed rows with attempts<max.
func (r *Runner[T]) feedQueue(ctx context.Context, opts RunOptions, queue chan<- map[string]any) error {
	where := r.workQueueWhere(opts)
	query := fmt.Sprintf(
		`SELECT * FROM items JOIN tasks USING (id) WHERE %s ORDER BY id`,
		where,
	)
	seen := map[string]int64{}

	for {
		// Materialize one batch fully before emitting. We hold exactly
		// one DuckDB connection (MaxOpenConns=1); if we pushed into the
		// bounded queue while still iterating rows, the conn would stay
		// pinned, workers would block on claim UPDATEs that need the
		// same conn, and the channel would back-pressure forever. Read
		// → Close → emit sidesteps the deadlock.
		batch, err := r.fetchBatch(ctx, query, seen)
		if err != nil {
			return err
		}

		for _, row := range batch {
			select {
			case queue <- row:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if len(batch) == 0 {
			return nil
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fetchBatch runs one work-queue select and returns the rows that passed
// the (id, attempts) dedup. The DuckDB conn is released before return,
// so callers are free to push into a bounded channel without risking
// starving the claim UPDATEs.
func (r *Runner[T]) fetchBatch(ctx context.Context, query string, seen map[string]int64) ([]map[string]any, error) {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: select work queue: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("taskrunner: columns: %w", err)
	}

	var batch []map[string]any
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("taskrunner: scan: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, name := range cols {
			row[name] = raw[i]
		}
		id, _ := row["id"].(string)
		attempts := asInt64FromAny(row["attempts"])
		if prev, ok := seen[id]; ok && attempts <= prev {
			continue
		}
		seen[id] = attempts
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("taskrunner: iterate: %w", err)
	}
	return batch, nil
}

// asInt64FromAny coerces a DuckDB row column value into int64 for the
// attempts field. DuckDB returns INTEGER as int32 via the go driver.
func asInt64FromAny(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

// workQueueWhere returns the unified WHERE fragment (built-in predicate
// AND user --where). Reserved state-column names in tasks prevent name
// collisions with items.
func (r *Runner[T]) workQueueWhere(opts RunOptions) string {
	builtin := fmt.Sprintf(
		`(status='pending'
		   OR (status='failed'  AND attempts<%d)
		   OR (status='running' AND (heartbeat_at IS NULL OR heartbeat_at < datetime('now', '-%d seconds'))))`,
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
		        started_at   = CURRENT_TIMESTAMP,
		        heartbeat_at = CURRENT_TIMESTAMP,
		        error        = NULL
		  WHERE id = ?
		    AND (status = 'pending'
		      OR (status = 'failed'  AND attempts < %d)
		      OR (status = 'running' AND (heartbeat_at IS NULL
		          OR heartbeat_at < datetime('now', '-%d seconds'))))`,
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
			`UPDATE tasks SET heartbeat_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
		return
	}
	buf, err := json.Marshal(status)
	if err != nil {
		return
	}
	_, _ = r.db.ExecContext(ctx,
		`UPDATE tasks
		    SET heartbeat_at = CURRENT_TIMESTAMP,
		        meta = json_patch(meta, json_object('status', json(?)))
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
				        ended_at     = CURRENT_TIMESTAMP,
				        heartbeat_at = CURRENT_TIMESTAMP,
				        error        = NULL,
				        meta = CASE WHEN ? IS NULL THEN meta
				                    ELSE json_patch(meta, json_object('result', json(?)))
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
		        ended_at     = CURRENT_TIMESTAMP,
		        heartbeat_at = CURRENT_TIMESTAMP,
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
