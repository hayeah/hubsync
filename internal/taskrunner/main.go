package taskrunner

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Main is the lightweight CLI wrapper: parse --resume, --where, --dry,
// --workers, --max-attempts, --stale, --heartbeat; construct a Runner
// from cfg; Execute; exit with code 0 on a clean drain, 1 on any
// remaining 'failed' rows, 2 on setup errors.
//
// Domain binaries that want custom flag handling / locks should call
// New + Execute themselves; Main is for quick-start CLIs.
func Main[T Task](cfg Config[T]) {
	fs := flag.NewFlagSet("task", flag.ExitOnError)
	resume := fs.String("resume", "", "path to the DuckDB task-state file (required)")
	where := fs.String("where", "", "SQL fragment AND-combined with the work-queue predicate")
	dry := fs.Bool("dry", false, "plan-if-needed, do not run any tasks")
	workers := fs.Int("workers", 0, "number of workers (default: runtime.NumCPU())")
	maxAttempts := fs.Int("max-attempts", 3, "retry ceiling for failed rows")
	stale := fs.Duration("stale", 30*time.Second, "running-row heartbeat staleness threshold")
	heartbeat := fs.Duration("heartbeat", 5*time.Second, "heartbeat / Status() poll interval")
	keepDB := fs.Bool("keep-db", false, "do not rename the DB file after a successful drain (default: archive in place)")
	_ = fs.Parse(os.Args[1:])

	if *resume == "" {
		fmt.Fprintln(os.Stderr, "task: --resume <path> is required")
		os.Exit(2)
	}

	r, err := New(Config[T]{
		DBPath: *resume,
		Plan:   cfg.Plan,
		Decode: cfg.Decode,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "task: %v\n", err)
		os.Exit(2)
	}
	defer r.Close()

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	opts := RunOptions{
		Workers:        *workers,
		Where:          *where,
		MaxAttempts:    *maxAttempts,
		Stale:          *stale,
		HeartbeatEvery: *heartbeat,
		Dry:            *dry,
		KeepDB:         *keepDB,
	}
	if err := r.Execute(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "task: %v\n", err)
		os.Exit(1)
	}

	if failed := r.Summary().Failed; failed > 0 {
		fmt.Fprintf(os.Stderr, "task: %d failed row(s) remain\n", failed)
		os.Exit(1)
	}
}
