// Package taskrunner implements the SQLite-backed task-runner pattern: a
// two-table (items, tasks) work queue with claim/heartbeat/stale-reclaim
// semantics.
//
// Usage shape:
//
//	type MyTask struct {
//	    ID string `json:"id"`
//	    // ...other fields serialized into items...
//	}
//
//	func (t *MyTask) Run(ctx context.Context) (any, error) { /* ... */ }
//
//	type MyFactory struct{ deps MyDeps }
//
//	func (f *MyFactory) List(ctx context.Context) iter.Seq2[taskrunner.Task, error] {
//	    return func(yield func(taskrunner.Task, error) bool) {
//	        // emit one task per unit of work
//	    }
//	}
//
//	func (f *MyFactory) Hydrate(ctx context.Context) (taskrunner.Task, error) {
//	    return &MyTask{ /* deps injected; JSON-tagged fields left zero */ }, nil
//	}
//
//	taskrunner.Main(taskrunner.Config{Factory: &MyFactory{deps: ...}})
//
// Plan / re-hydrate lifecycle:
//
//	List    — once per DB lifetime (first Execute when items is empty);
//	          the runner binds each yielded Task into the items table and
//	          seeds tasks from items.
//	Hydrate — once per claim (every successful row pickup); the runner
//	          subsequently json.Unmarshal's the items row into the returned
//	          Task (JSON-tagged fields populate; `json:"-"` deps survive).
package taskrunner

import (
	"context"
	"iter"
)

// Task is the unit of work. Concrete types carry both the row body
// (must round-trip through encoding/json with an "id" field) and the
// executor (Run). The runner never touches a Task beyond marshaling at
// plan/hydrate time and invoking Run.
type Task interface {
	// Run executes one task. The returned result is JSON-marshaled and
	// merged into tasks.meta.result on success. A non-nil err becomes a
	// 'failed' terminal write.
	//
	// Handlers MUST be idempotent — a stale-heartbeat running row is
	// re-claimed from scratch.
	Run(ctx context.Context) (result any, err error)
}

// StatusReporter is an optional add-on probed via type assertion: when
// a Task implements it, the runner polls Status on each heartbeat tick
// and merges the snapshot into tasks.meta.status. Called concurrently
// with Run; handler must make shared state access safe (atomic, mutex).
type StatusReporter interface {
	Status() (any, error)
}

// TaskFactory produces tasks. List enumerates work once on first run;
// Hydrate returns a fresh Task instance for every worker claim.
//
// Implementations typically close over a dependency struct (storage
// client, DB handle, paths) once and re-use on every call. The factory
// itself is long-lived — it spans the Runner's lifetime.
type TaskFactory interface {
	// List enumerates work on first run. Called exactly when the items
	// table is empty (first Execute, or after a manual wipe). Returns a
	// range-over-func iterator: each yielded (Task, nil) is planned into
	// the items table; a yielded (_, err) aborts planning with that
	// error.
	//
	// The ctx is the caller's Execute ctx — honor it for cancellation
	// on long enumerations (directory walks, DB queries, remote paging).
	List(ctx context.Context) iter.Seq2[Task, error]

	// Hydrate returns a fresh Task instance with transient deps injected.
	// Called once per successful claim. The runner subsequently
	// json.Unmarshal's the items row into the returned Task, so any
	// JSON-tagged fields are overwritten from persisted state; deps
	// tagged `json:"-"` survive the unmarshal.
	//
	// An error here becomes a terminal 'failed' write on the row with
	// "decode: <err>", matching the prior Decode-error semantics.
	Hydrate(ctx context.Context) (Task, error)
}
