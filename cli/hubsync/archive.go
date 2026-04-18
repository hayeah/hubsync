package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/hayeah/hubsync"
)

// cmdArchive is the one-shot `hubsync archive [dir]` verb. Walks a hub
// directory and uploads every pending row to B2. With --dry, just lists
// what would upload as JSONL (or a table on a TTY) — no network, no state
// changes to B2.
//
// Exit codes:
//   0 — all uploads succeeded (or nothing to do, or --dry succeeded).
//   1 — one or more uploads failed; remaining files were still attempted.
//   2 — startup failure (no .hubsync, bad config, lock held, ...).
func cmdArchive(args []string) {
	fs := flag.NewFlagSet("archive", flag.ExitOnError)
	dry := fs.Bool("dry", false, "list pending uploads as JSONL and exit without touching B2")
	quiet := fs.Bool("quiet", false, "suppress per-file progress on stderr")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hubsync archive [dir] [--dry] [--quiet]\n\nOne-shot: scan the hub dir and upload every pending file to B2, then exit.\n")
	}
	fs.Parse(args)

	dir := "."
	switch fs.NArg() {
	case 0:
	case 1:
		dir = fs.Arg(0)
	default:
		fs.Usage()
		os.Exit(2)
	}

	hubDir, err := locateHubDir(dir)
	if err != nil {
		fatalf("%v", err)
	}

	// --dry is pure read: no B2 credentials required. Only the upload path
	// needs the storage client.
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

	if *dry {
		if err := runArchiveDry(stack); err != nil {
			fatalf("archive --dry: %v", err)
		}
		return
	}

	if err := runArchiveUpload(ctx, stack, *quiet); err != nil {
		os.Exit(err.(*archiveExit).code)
	}
}

// archiveExit is used to plumb a specific exit code out of runArchiveUpload
// without log.Fatal semantics.
type archiveExit struct {
	code int
}

func (e *archiveExit) Error() string { return fmt.Sprintf("archive exit %d", e.code) }

// runArchiveDry emits pending rows in the shared ls JSONL shape.
func runArchiveDry(stack *oneShotStack) error {
	rows, err := stack.store.PendingArchiveRows()
	if err != nil {
		return err
	}
	renderLs(os.Stdout, rows)
	return nil
}

// runArchiveUpload runs RunOnce, streams per-file progress to stderr,
// prints a summary, and returns *archiveExit with a non-zero code on
// failures (or nil on full success).
func runArchiveUpload(ctx context.Context, stack *oneShotStack, quiet bool) error {
	var mu sync.Mutex
	progress := func(u hubsync.ArchiveUpload, upErr error) {
		if quiet {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if upErr != nil {
			fmt.Fprintf(os.Stderr, "archiving %s (%s)... FAIL: %v\n", u.Path, formatBytes(u.Size), upErr)
			return
		}
		fmt.Fprintf(os.Stderr, "archiving %s (%s)... ok\n", u.Path, formatBytes(u.Size))
	}
	result, err := stack.archiveWorker.RunOnce(ctx, progress)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("archive: %v", err)
	}
	fmt.Fprintf(os.Stderr, "archived %d files (%s), %d failed\n",
		len(result.Uploaded), formatBytes(result.TotalBytes()), len(result.Failed))
	if len(result.Failed) > 0 {
		return &archiveExit{code: 1}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return &archiveExit{code: 1}
	}
	return nil
}

