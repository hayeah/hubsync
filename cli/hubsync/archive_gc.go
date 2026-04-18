package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hayeah/hubsync"
)

// cmdArchiveGC is the one-shot `hubsync archive-gc <prefix>` verb. It lists
// a B2 prefix one delimiter-level at a time, classifies each entry against
// hub_entry claims, and (unless --dry) deletes orphans.
//
// Read-only against hub.db (SQLite WAL lets us coexist with a running
// serve), so it does NOT take the hub lock. Writes to B2 are guarded per-
// object by the fileID race-guard in DeleteByKey.
//
// Exit codes:
//
//	0 — success (all orphans deleted, or --dry succeeded).
//	1 — one or more deletes failed; others were still attempted.
//	2 — startup failure (no .hubsync, [archive] missing, bad prefix, etc.).
func cmdArchiveGC(args []string) {
	fs := flag.NewFlagSet("archive-gc", flag.ExitOnError)
	dry := fs.Bool("dry", false, "classify without deleting; emit JSONL of would-be actions")
	dir := fs.String("dir", ".", "hub directory (searches up for .hubsync)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hubsync archive-gc <prefix> [--dry] [-dir DIR]\n\nList a B2 prefix and delete objects that no hub_entry row claims.\nPrefix is a literal bucket-relative string (never a glob).\n")
	}
	fs.Parse(args)

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	prefix := fs.Arg(0)

	hubDir, err := locateHubDir(*dir)
	if err != nil {
		fatalf("%v", err)
	}

	cfg, err := hubsync.LoadConfigFile(hubDir)
	if err != nil {
		fatalf("load config: %v", err)
	}
	if cfg.Archive == nil {
		fatalf("archive not configured — add an [archive] section to %s/.hubsync/config.toml", hubDir)
	}

	store, storeCleanup, err := openHubStoreRO(hubDir)
	if err != nil {
		fatalf("%v", err)
	}
	defer storeCleanup()

	storage, storageCleanup, err := hubsync.ProvideArchiveStorage(cfg)
	if err != nil {
		fatalf("open archive storage: %v", err)
	}
	defer storageCleanup()

	gc := &hubsync.ArchiveGC{
		Store:   store,
		Storage: storage,
		Prefix:  cfg.Archive.BucketPrefix,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	enc := json.NewEncoder(os.Stdout)
	summary, err := gc.Run(ctx, prefix, *dry, func(e hubsync.ArchiveGCEntry) {
		_ = enc.Encode(e)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-gc: %v\n", err)
		os.Exit(1)
	}

	// One-line human summary on stderr.
	if *dry {
		fmt.Fprintf(os.Stderr,
			"archive-gc (dry): %d orphan file(s), %d orphan dir(s); %d kept file(s), %d kept dir(s)\n",
			summary.OrphanFiles, summary.OrphanDirs, summary.KeptFiles, summary.KeptDirs)
	} else {
		fmt.Fprintf(os.Stderr,
			"archive-gc: deleted %d file(s) (%s); %d orphan dir(s) swept; %d kept file(s), %d kept dir(s); %d error(s)\n",
			summary.DeletedFiles, formatBytes(summary.DeletedBytes),
			summary.OrphanDirs, summary.KeptFiles, summary.KeptDirs, summary.Errors)
	}

	if summary.Errors > 0 {
		os.Exit(1)
	}
}
