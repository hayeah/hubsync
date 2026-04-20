package taskrunner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Config wires a TaskFactory to the runner. Non-generic so future
// additions (e.g. a Docstring template) land without re-parametrizing.
type Config struct {
	// Factory supplies both the plan-time emitter (List) and the
	// per-claim allocator (Hydrate). Required.
	Factory TaskFactory
}

// RunOptions tunes a single run. DBPath lives here (not on Config)
// because it's a runtime knob owned by --resume.
type RunOptions struct {
	// DBPath is the SQLite file path. Empty uses an in-memory database.
	// The file is opened in WAL journal mode so external readers (e.g.
	// `sqlite3 file.db "select … from tasks"`) can observe task state
	// while a writer is running.
	//
	// New reads this at construction time; Execute ignores it
	// (reconfiguring the DB mid-Runner is not supported).
	DBPath         string
	Workers        int           // default runtime.NumCPU()
	Where          string        // SQL fragment AND-combined with work-queue predicate
	MaxAttempts    int           // default 3
	Stale          time.Duration // default 30s
	HeartbeatEvery time.Duration // default 5s
	Dry            bool          // plan-if-empty, don't run

	// KeepDB disables archive-on-success. When false (default) and
	// Execute drains cleanly with zero remaining pending/failed/running
	// rows, the DB file is closed and renamed in place with a UTC
	// ISO-8601 timestamp prefix so the next invocation at the canonical
	// path plans from scratch. Set KeepDB to preserve same-path resume
	// semantics.
	KeepDB bool
}

// Runner owns the DB handle, the cached item schema, and the
// serialized-write mutex.
type Runner struct {
	cfg    Config
	dbPath string // captured from opts at New time; archiveIfDrained needs it
	db     *sql.DB
	schema itemSchema // cached at New; used by plan + hydrate paths

	// writeMu serializes writes within this process. SQLite in WAL mode
	// allows concurrent readers with a single writer; busy_timeout
	// absorbs cross-process contention, and writeMu spares intra-process
	// writes from that retry loop.
	writeMu sync.Mutex

	// planErr captures the first error observed during the plan loop
	// (bind / INSERT failures; wrong-type yields).
	planErr error

	// closed guards against double-close after archive-on-success already
	// closed the handle.
	closed bool

	// archivedPath is set by archiveIfDrained after a successful rename.
	archivedPath string

	// summary is the last-observed tasks-table snapshot, captured at the
	// end of Execute so callers that want a terminal status line can
	// read it even after archive-on-success has closed the DB.
	summary Summary
}

// Summary is a snapshot of the tasks table grouped by status.
type Summary struct {
	Pending int
	Running int
	Done    int
	Failed  int
}

// RunResult is the return value of Run. Zero-valued on setup errors
// (where Run couldn't open the DB or probe the factory).
type RunResult struct {
	Summary      Summary
	ArchivedPath string
}

// Error sentinels. Callers branch on these via errors.Is:
//
//   - ErrSetup wraps errors encountered before Execute starts (DB open,
//     schema inference, factory.Hydrate probe). Main maps this to exit 2.
//   - ErrFailedRemain is returned from Run on a clean drain where the
//     tasks table still has rows in terminal 'failed' status. Main maps
//     this to exit 1.
var (
	ErrSetup        = errors.New("taskrunner: setup failed")
	ErrFailedRemain = errors.New("taskrunner: failed row(s) remain")
)

// New opens the SQLite DB, probes the factory to infer the items
// schema, and returns a Runner ready for Execute. Callers that only
// want the packaged run-loop should prefer Run.
//
// The factory.Hydrate(ctx) probe at New time is how we cache the items
// schema; implementations that require a fully-populated ctx for that
// call can either pass the caller's ctx through or (typical case)
// return a bare struct with deps pre-wired and ignore ctx.
func New(ctx context.Context, cfg Config, opts RunOptions) (*Runner, error) {
	if cfg.Factory == nil {
		return nil, fmt.Errorf("%w: Config.Factory is required", ErrSetup)
	}
	sample, err := cfg.Factory.Hydrate(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: factory.Hydrate probe: %w", ErrSetup, err)
	}
	schema, err := inferItemSchema(sample)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSetup, err)
	}
	dsn := sqliteDSN(opts.DBPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %w", ErrSetup, opts.DBPath, err)
	}
	if err := ensureTasksTable(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: %w", ErrSetup, err)
	}
	return &Runner{cfg: cfg, dbPath: opts.DBPath, db: db, schema: schema}, nil
}

// sqliteDSN builds a modernc.org/sqlite DSN that enables WAL, a 5s
// busy_timeout, and foreign_keys. An empty path maps to a shared-cache
// in-memory DB so multiple *sql.DB handles from the same process see
// the same data during tests.
func sqliteDSN(path string) string {
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	if path == "" {
		return "file::memory:?cache=shared&" + pragmas
	}
	return "file:" + path + "?" + pragmas
}

// Close releases the DB handle. Idempotent.
func (r *Runner) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.db.Close()
}

// ArchivedPath returns the destination of the archived DB if Execute's
// drain completed with an empty queue and KeepDB was false.
func (r *Runner) ArchivedPath() string { return r.archivedPath }

// Summary returns the last-observed tasks-table snapshot captured at
// the end of Execute.
func (r *Runner) Summary() Summary { return r.summary }

// DB exposes the underlying handle for tests and introspection.
// Writes should still go through the runner's mutex.
func (r *Runner) DB() *sql.DB { return r.db }

// Run is the packaged entry point: New → Execute → capture summary →
// Close. Does not install signal handlers; does not call os.Exit.
// Caller owns both.
//
// Error composition:
//   - Setup failures wrap ErrSetup and are returned with zero RunResult.
//   - Execute errors return wrapped err with populated RunResult.
//   - A clean drain with Summary.Failed > 0 returns ErrFailedRemain
//     with populated RunResult.
//   - A clean drain with no failures returns nil.
func Run(ctx context.Context, cfg Config, opts RunOptions) (RunResult, error) {
	r, err := New(ctx, cfg, opts)
	if err != nil {
		return RunResult{}, err
	}
	defer r.Close()

	execErr := r.Execute(ctx, opts)

	result := RunResult{
		Summary:      r.Summary(),
		ArchivedPath: r.ArchivedPath(),
	}
	if execErr != nil {
		return result, execErr
	}
	if result.Summary.Failed > 0 {
		return result, ErrFailedRemain
	}
	return result, nil
}

// BindFlags binds the taskrunner's flags onto fs and returns the
// RunOptions the caller reads after fs.Parse. Callers may add their
// own flags to the same fs before parsing.
//
// Bound flags: --resume (→ DBPath), --where, --dry, --workers,
// --max-attempts, --stale, --heartbeat, --keep-db.
func BindFlags(fs *flag.FlagSet) *RunOptions {
	var opts RunOptions
	fs.StringVar(&opts.DBPath, "resume", "", "path to the SQLite task-state file (empty → in-memory)")
	fs.StringVar(&opts.Where, "where", "", "SQL fragment AND-combined with the work-queue predicate")
	fs.BoolVar(&opts.Dry, "dry", false, "plan-if-needed, do not run any tasks")
	fs.IntVar(&opts.Workers, "workers", 0, "number of workers (default: runtime.NumCPU())")
	fs.IntVar(&opts.MaxAttempts, "max-attempts", 3, "retry ceiling for failed rows")
	fs.DurationVar(&opts.Stale, "stale", 30*time.Second, "running-row heartbeat staleness threshold")
	fs.DurationVar(&opts.HeartbeatEvery, "heartbeat", 5*time.Second, "heartbeat / Status() poll interval")
	fs.BoolVar(&opts.KeepDB, "keep-db", false, "do not rename the DB file after a successful drain (default: archive in place)")
	return &opts
}

// Execute is the one-verb operation: plan-if-needed, select unfinished
// rows (+ user --where), and drain them through a worker pool. The
// DBPath field on opts is ignored — that path is captured at New time.
func (r *Runner) Execute(ctx context.Context, opts RunOptions) error {
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

	if err := r.drain(ctx, opts); err != nil {
		return err
	}

	if err := r.captureSummary(ctx); err != nil {
		return err
	}

	return r.archiveIfDrained(ctx, opts)
}

// captureSummary reads the final tasks-table counts and stashes them on
// the runner so callers can read them even after archive-on-success has
// closed the DB handle.
func (r *Runner) captureSummary(ctx context.Context) error {
	var s Summary
	err := r.db.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status='running' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status='done'    THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status='failed'  THEN 1 ELSE 0 END), 0)
		 FROM tasks`).Scan(&s.Pending, &s.Running, &s.Done, &s.Failed)
	if err != nil {
		return fmt.Errorf("taskrunner: capture summary: %w", err)
	}
	r.summary = s
	return nil
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
func (r *Runner) drain(ctx context.Context, opts RunOptions) error {
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
// and terminal-wrote 'failed').
func (r *Runner) feedQueue(ctx context.Context, opts RunOptions, queue chan<- map[string]any) error {
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

// fetchBatch runs one work-queue select and returns rows that passed
// the (id, attempts) dedup.
func (r *Runner) fetchBatch(ctx context.Context, query string, seen map[string]int64) ([]map[string]any, error) {
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

// asInt64FromAny coerces a column value into int64 for attempts.
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

// workQueueWhere returns the unified WHERE fragment.
func (r *Runner) workQueueWhere(opts RunOptions) string {
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

// processRow claims one row, hydrates it into a Task, runs it (with
// heartbeat), and writes the terminal status.
func (r *Runner) processRow(ctx context.Context, row map[string]any, opts RunOptions) error {
	id, _ := row["id"].(string)
	if id == "" {
		return fmt.Errorf("taskrunner: row missing id")
	}

	claimed, err := r.claim(ctx, id, opts)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	task, hydrateErr := r.hydrate(ctx, row)
	if hydrateErr != nil {
		return r.writeTerminal(ctx, id, "failed", fmt.Errorf("decode: %w", hydrateErr), nil)
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

// hydrate calls factory.Hydrate(ctx), then json-round-trips the items
// row into the returned Task. Complex columns stored as TEXT-of-JSON
// are re-wrapped as json.RawMessage so they unmarshal as structured
// values rather than re-wrapping as JSON-inside-a-string.
func (r *Runner) hydrate(ctx context.Context, row map[string]any) (Task, error) {
	task, err := r.cfg.Factory.Hydrate(ctx)
	if err != nil {
		return nil, err
	}
	obj := make(map[string]any, len(r.schema.columns))
	for _, c := range r.schema.columns {
		v, ok := row[c.name]
		if !ok || v == nil {
			continue
		}
		if c.isJSON {
			switch x := v.(type) {
			case string:
				obj[c.name] = json.RawMessage(x)
			case []byte:
				obj[c.name] = json.RawMessage(x)
			default:
				obj[c.name] = v
			}
		} else {
			obj[c.name] = v
		}
	}
	buf, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal row: %w", err)
	}
	if err := json.Unmarshal(buf, task); err != nil {
		return nil, fmt.Errorf("unmarshal into task: %w", err)
	}
	return task, nil
}

// claim atomically flips an eligible row to 'running'.
func (r *Runner) claim(ctx context.Context, id string, opts RunOptions) (bool, error) {
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

// heartbeatLoop refreshes heartbeat_at on a ticker. StatusReporter is
// probed via type assertion.
func (r *Runner) heartbeatLoop(ctx context.Context, id string, task Task, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	reporter, hasStatus := task.(StatusReporter)

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

func (r *Runner) beat(ctx context.Context, id string, status any) {
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

// writeTerminal commits 'done' or 'failed' for a task.
func (r *Runner) writeTerminal(ctx context.Context, id, status string, runErr error, result any) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if status == "done" {
		var resultJSON string
		if result != nil {
			buf, err := json.Marshal(result)
			if err != nil {
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

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// schemaType is exposed for the plan-loop's emit-type validation.
func (r *Runner) schemaType() reflect.Type { return r.schema.schemaType }
