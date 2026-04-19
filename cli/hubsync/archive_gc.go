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
	"github.com/hayeah/hubsync/archive"
)

// cmdArchiveGC is the one-shot `hubsync archive-gc [path]` verb. It lists
// a B2 prefix one delimiter-level at a time, classifies each entry against
// hub_entry claims, and (unless --dry) deletes orphans.
//
// The path arg is cwd-relative within the hub tree (same semantics as `ls`);
// the hub's bucket_prefix is automatically prepended when composing the B2
// list prefix. Bare `archive-gc` or `archive-gc .` lists the bucket_prefix
// root at cwd's position.
//
// Read-only against hub.db (SQLite WAL lets us coexist with a running
// serve), so it does NOT take the hub lock. Writes to B2 are guarded per-
// object by the fileID race-guard in DeleteByKey.
//
// Exit codes:
//
//	0 — success (all orphans deleted, or --dry succeeded).
//	1 — one or more deletes failed; others were still attempted.
//	2 — startup failure (no .hubsync, [archive] missing, path escapes hub, etc.).
func cmdArchiveGC(args []string) {
	fs := flag.NewFlagSet("archive-gc", flag.ExitOnError)
	dry := fs.Bool("dry", false, "classify without deleting; emit JSONL of would-be actions")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hubsync archive-gc [path] [--dry]\n\nList a B2 prefix (cwd-relative inside the hub; bucket_prefix auto-prepended)\nand delete objects that no hub_entry row claims.\n")
	}
	fs.Parse(args)

	arg := "."
	switch fs.NArg() {
	case 0:
	case 1:
		arg = fs.Arg(0)
	default:
		fs.Usage()
		os.Exit(2)
	}

	hc, err := hubContext()
	if err != nil {
		fatalf("%v", err)
	}
	relPrefix, err := hc.resolvePrefix(arg)
	if err != nil {
		fatalf("archive-gc: %v", err)
	}

	cfg, err := hubsync.LoadConfigFile(hc.root)
	if err != nil {
		fatalf("load config: %v", err)
	}
	if cfg.Archive == nil {
		fatalf("archive not configured — add an [archive] section to %s/.hubsync/config.toml", hc.root)
	}

	store, storeCleanup, err := openHubStoreRO(hc.root)
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

	// Compose the B2 list prefix: configured bucket_prefix + hub-relative prefix.
	bucketPrefix := archive.JoinKey(cfg.Archive.BucketPrefix, relPrefix)

	enc := json.NewEncoder(os.Stdout)
	summary, err := gc.Run(ctx, bucketPrefix, *dry, func(e hubsync.ArchiveGCEntry) {
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
