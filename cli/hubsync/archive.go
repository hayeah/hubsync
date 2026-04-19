package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hayeah/hubsync"
	"github.com/hayeah/hubsync/internal/taskrunner"
)

// cmdArchive is the one-shot `hubsync archive` verb.
//
// Execution model follows the DuckDB task-runner pattern:
//
//	hubsync archive                                     # resume default archive.duckdb
//	hubsync archive --resume <path>                     # explicit state file
//	hubsync archive --dry                               # plan + populate items, don't run
//	hubsync archive --where "path LIKE '%.mov'"         # subset by items column
//	hubsync archive --where "attempts = 0"              # subset by tasks column
//	hubsync archive --workers 8
//
// The state file lives at `.hubsync/archive.duckdb` by default. It's
// the canonical artifact: every invocation is the same operation
// (plan-if-empty + run work queue), distinguished only by the DB's
// current state and the user's --where.
//
// Exit codes:
//
//	0 — queue drained clean.
//	1 — one or more tasks remain in 'failed'. Other tasks still ran.
//	2 — startup failure (no .hubsync, bad config, lock held, ...).
func cmdArchive(args []string) {
	fs := flag.NewFlagSet("archive", flag.ExitOnError)
	resume := fs.String("resume", "", "DuckDB task-state file (default: <hub>/.hubsync/archive.duckdb)")
	where := fs.String("where", "", "SQL fragment AND-combined with the work-queue predicate")
	dry := fs.Bool("dry", false, "plan-if-empty; do not upload")
	workers := fs.Int("workers", 0, "number of concurrent upload workers")
	maxAttempts := fs.Int("max-attempts", 3, "retry ceiling for failed tasks")
	stale := fs.Duration("stale", 30*time.Second, "heartbeat staleness threshold for stale-running reclaim")
	heartbeat := fs.Duration("heartbeat", 5*time.Second, "heartbeat interval")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: hubsync archive [--resume <path>] [--dry] [--where "<sql>"] [--workers N]

Plans (if needed) + runs the archive task queue. The DuckDB file at
--resume is the canonical artifact; every invocation is plan-if-empty
followed by a drain of the work queue, optionally filtered by --where.

Examples:
  hubsync archive                                       # full run
  hubsync archive --dry                                 # plan only
  hubsync archive --where "path LIKE '%%.mov'"          # subset
  hubsync archive --where "attempts = 0"                # only never-tried
`)
	}
	fs.Parse(args)

	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}

	hc, err := hubContext()
	if err != nil {
		fatalf("%v", err)
	}
	hubDir := hc.root

	// --dry is pure-local: no B2 credentials needed.
	needArchive := !*dry
	stack, release, err := acquireOneShot(hubDir, needArchive)
	if err != nil {
		if errors.Is(err, hubsync.ErrLocked) {
			fatalf("hub at %s is locked by another hubsync process (%s); stop `hubsync serve` or the other `hubsync archive` first", hubDir, hubsync.HubLockPath(hubDir))
		}
		fatalf("%v", err)
	}
	defer release()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Fresh scan so pending rows reflect current disk state.
	if err := stack.hub.FullScan(); err != nil {
		fatalf("scan %s: %v", hubDir, err)
	}

	dbPath := *resume
	if dbPath == "" {
		dbPath = filepath.Join(hubDir, ".hubsync", "archive.duckdb")
	}

	deps := hubsync.ArchiveTaskDeps{
		Store:   stack.store,
		Storage: stack.storage, // may be nil on --dry; taskrunner never calls Run in that case
		Hasher:  stack.hasher,
		HubDir:  hubDir,
		Prefix:  archivePrefix(stack),
	}

	cfg := taskrunner.Config[*hubsync.ArchiveTask]{
		DBPath: dbPath,
		Plan:   hubsync.PlanArchiveTasks(deps),
		Decode: hubsync.DecodeArchiveTask(deps),
	}

	runner, err := taskrunner.New(cfg)
	if err != nil {
		fatalf("taskrunner: %v", err)
	}
	defer runner.Close()

	opts := taskrunner.RunOptions{
		Workers:        *workers,
		Where:          *where,
		MaxAttempts:    *maxAttempts,
		Stale:          *stale,
		HeartbeatEvery: *heartbeat,
		Dry:            *dry,
	}
	if err := runner.Execute(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "archive: %v\n", err)
		os.Exit(1)
	}

	summary, err := summarizeTasks(runner)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive: summary: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, summary)
	if hasFailed(runner) {
		os.Exit(1)
	}
}

// archivePrefix pulls cfg.Archive.BucketPrefix if [archive] is configured;
// returns "" if the hub has no archive section (--dry mode without creds).
func archivePrefix(s *oneShotStack) string {
	if s.config == nil || s.config.Archive == nil {
		return ""
	}
	return s.config.Archive.BucketPrefix
}

// summarizeTasks returns a one-line summary of the tasks table state.
func summarizeTasks(r *taskrunner.Runner[*hubsync.ArchiveTask]) (string, error) {
	var pending, running, done, failed int
	err := r.DB().QueryRow(
		`SELECT
		   COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status='running' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status='done'    THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status='failed'  THEN 1 ELSE 0 END), 0)
		 FROM tasks`).Scan(&pending, &running, &done, &failed)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("archive: %d done, %d pending, %d running, %d failed",
		done, pending, running, failed), nil
}

func hasFailed(r *taskrunner.Runner[*hubsync.ArchiveTask]) bool {
	var n int
	_ = r.DB().QueryRow(`SELECT COUNT(*) FROM tasks WHERE status='failed'`).Scan(&n)
	return n > 0
}
