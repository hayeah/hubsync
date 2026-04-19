// Package taskrunner implements the DuckDB-backed task-runner pattern: a
// two-table (items, tasks) work queue with claim/heartbeat/stale-reclaim
// semantics. The pattern is specified in detail at
// ~/Dropbox/notes/2026-04-19/task-runner-duckdb-spec_claude.md.
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
//	taskrunner.Main[*MyTask](taskrunner.Config[*MyTask]{
//	    Plan:   func(ctx, emit) error { /* emit(&MyTask{...}) */ },
//	    Decode: func(row map[string]any) (*MyTask, error) { /* ... */ },
//	})
//
// Keep the package self-contained: callers wire in domain logic via Plan
// + Decode + the Task.Run method, and the runner owns schema, claim, and
// heartbeat.
package taskrunner

import "context"

// Task is the unit of work. Concrete T is both the row body (it must
// round-trip through encoding/json with an "id" field) and the executor
// (Run). The runner never touches T beyond marshaling at plan time and
// invoking Run.
type Task interface {
	// Run executes one task. The returned result is JSON-marshaled and
	// merged into tasks.meta.result on success. A non-nil err becomes a
	// 'failed' terminal write.
	//
	// Handlers MUST be idempotent — after a crash, a 'running' row with
	// a stale heartbeat is re-claimed and Run is called again from
	// scratch. The runner does not pass breadcrumbs; smarter recovery
	// (resume-at-offset, dedup-by-key) is the handler's responsibility.
	Run(ctx context.Context) (result any, err error)
}

// StatusReporter is an optional add-on: when T implements it, the runner
// polls Status on each heartbeat tick and merges the snapshot into
// tasks.meta.status. Called concurrently with Run; handler must make
// shared state access safe (atomic, mutex).
type StatusReporter interface {
	Status() (any, error)
}
