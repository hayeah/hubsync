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

	"github.com/hayeah/hubsync"
	"github.com/hayeah/hubsync/internal/taskrunner"
)

// cmdArchive is the one-shot `hubsync archive` verb.
//
// Execution model follows the taskrunner pattern:
//
//	hubsync archive                                     # resume default archive.sqlite
//	hubsync archive --resume <path>                     # explicit state file
//	hubsync archive --dry                               # plan + populate items, don't run
//	hubsync archive --where "path LIKE '%.mov'"         # subset by items column
//	hubsync archive --where "attempts = 0"              # subset by tasks column
//	hubsync archive --workers 8
//
// The state file lives at `.hubsync/archive.duckdb` by default.
//
// Exit codes:
//
//	0 — queue drained clean.
//	1 — one or more tasks remain in 'failed'. Other tasks still ran.
//	2 — startup failure (no .hubsync, bad config, lock held, ...).
func cmdArchive(args []string) {
	fs := flag.NewFlagSet("archive", flag.ExitOnError)
	opts := taskrunner.BindFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: hubsync archive [--resume <path>] [--dry] [--where "<sql>"] [--workers N]

Plans (if needed) + runs the archive task queue. The SQLite file at
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
	needArchive := !opts.Dry
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

	if opts.DBPath == "" {
		opts.DBPath = filepath.Join(hubDir, ".hubsync", "archive.duckdb")
	}

	deps := hubsync.ArchiveTaskDeps{
		Store:   stack.store,
		Storage: stack.storage, // may be nil on --dry; Run is never called in that case
		Hasher:  stack.hasher,
		HubDir:  hubDir,
		Prefix:  archivePrefix(stack),
	}

	cfg := taskrunner.Config{
		Factory: hubsync.NewArchiveTaskFactory(deps),
	}

	result, err := taskrunner.Run(ctx, cfg, *opts)
	fmt.Fprintf(os.Stderr, "archive: %d done, %d pending, %d running, %d failed\n",
		result.Summary.Done, result.Summary.Pending, result.Summary.Running, result.Summary.Failed)
	if result.ArchivedPath != "" {
		fmt.Fprintf(os.Stderr, "archive: archived task DB -> %s\n", result.ArchivedPath)
	}
	if err != nil {
		if !errors.Is(err, taskrunner.ErrFailedRemain) {
			fmt.Fprintf(os.Stderr, "archive: %v\n", err)
		}
		if errors.Is(err, taskrunner.ErrSetup) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// archivePrefix pulls cfg.Archive.BucketPrefix if [archive] is configured.
func archivePrefix(s *oneShotStack) string {
	if s.config == nil || s.config.Archive == nil {
		return ""
	}
	return s.config.Archive.BucketPrefix
}
