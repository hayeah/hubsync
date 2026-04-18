package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hayeah/hubsync"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "client":
		cmdClient(os.Args[2:])
	case "archive":
		cmdArchive(os.Args[2:])
	case "archive-gc":
		cmdArchiveGC(os.Args[2:])
	case "pin":
		cmdPin(os.Args[2:], hubsync.TargetArchived)
	case "unpin":
		cmdPin(os.Args[2:], hubsync.TargetUnpinned)
	case "ls":
		cmdLs(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `hubsync — hub-and-client file sync with B2 archive

Usage:
  hubsync init     [dir]                      Scaffold .hubsync/config.toml under dir (default: .).
  hubsync serve    [flags]                    Run the hub (scanner + watcher + archive + RPC).
  hubsync client   [flags]                    Run a replica (read or write mode).
  hubsync archive  [dir] [--dry] [--quiet]    One-shot: scan + upload pending files to B2, then exit.
  hubsync archive-gc <prefix> [--dry]         List a B2 prefix and delete objects no hub_entry claims.
  hubsync pin      <glob>... [--dry]          Ensure matched paths are archived (remote + local).
  hubsync unpin    <glob>... [--dry]          Ensure matched paths are unpinned (remote only).
  hubsync ls       [--quiet]                  List every tree entry (JSONL when piped, table on TTY).
  hubsync status   [--json]                   Tree + archive counts.

'archive', 'pin', 'unpin', 'ls', 'status' work with or without a running
'hubsync serve': if serve is up, they RPC to .hubsync/serve.sock; otherwise
they run in-process after taking an exclusive flock on .hubsync/hub.lock.
`)
}

// --- init ----------------------------------------------------------------

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hubsync init [dir]\n\nScaffold .hubsync/config.toml under dir (default: current directory).\nRefuses to overwrite an existing config; remove it by hand to re-init.\n")
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

	path, err := runInit(dir)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s\n", path)
}

// --- serve ---------------------------------------------------------------

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory to watch")
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	dbPath := fs.String("db", "", "database path (default: <dir>/.hubsync/hub.db)")
	fs.Parse(args)

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("resolve dir: %v", err)
	}

	if *dbPath == "" {
		*dbPath = filepath.Join(absDir, ".hubsync", "hub.db")
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	// Exclusive lock — blocks a second serve, or an `archive`/in-process
	// pin/unpin, from touching this hub while we run.
	lock, err := hubsync.AcquireHubLock(absDir)
	if err != nil {
		log.Fatalf("acquire hub lock: %v", err)
	}
	defer lock.Release()

	app, cleanup, err := InitializeHubApp(&hubsync.HubConfig{
		DBPath:   *dbPath,
		WatchDir: absDir,
		Token:    os.Getenv("HUBSYNC_TOKEN"),
		Listen:   *listen,
	})
	if err != nil {
		log.Fatalf("initialize: %v", err)
	}
	defer cleanup()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Hub scanner/watcher.
	go func() {
		if err := app.Hub.Start(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("hub error: %v", err)
		}
	}()

	// Archive worker (only if [archive] configured).
	if app.ArchiveWorker != nil {
		go func() {
			if err := app.ArchiveWorker.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("archive worker exited: %v", err)
			}
		}()
	}

	// RPC server.
	if app.RPC != nil {
		go func() {
			if err := app.RPC.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
				log.Printf("rpc server exited: %v", err)
			}
		}()
	}

	// HTTP server (sync/subscribe, /blobs, /push, /snapshots-tree).
	go func() {
		if err := app.Server.ListenAndServe(); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
}

// --- client --------------------------------------------------------------

func cmdClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	hubURL := fs.String("hub", "", "hub URL (required)")
	dir := fs.String("dir", ".", "directory to sync to")
	dbPath := fs.String("db", "", "database path (default: <dir>/.hubsync/client.db)")
	mode := fs.String("mode", "read", "sync mode: read or write")
	scanInterval := fs.String("scan-interval", "5s", "scan interval for write mode")
	once := fs.Bool("once", false, "bootstrap and/or catch up to the hub's current state, then exit")
	fs.Parse(args)

	if *hubURL == "" {
		log.Fatal("-hub is required")
	}

	if *mode != "read" && *mode != "write" {
		log.Fatalf("-mode must be 'read' or 'write', got %q", *mode)
	}

	if *once && *mode == "write" {
		log.Fatal("-once is not supported with -mode write")
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("resolve dir: %v", err)
	}

	if *dbPath == "" {
		*dbPath = filepath.Join(absDir, ".hubsync", "client.db")
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	app, cleanup, err := InitializeClientApp(&hubsync.ClientConfig{
		DBPath:       *dbPath,
		HubURL:       *hubURL,
		Token:        os.Getenv("HUBSYNC_TOKEN"),
		SyncDir:      absDir,
		Mode:         *mode,
		ScanInterval: *scanInterval,
	})
	if err != nil {
		log.Fatalf("initialize: %v", err)
	}
	defer cleanup()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *once {
		log.Printf("catching up %s from %s", absDir, *hubURL)
		if err := app.Client.Catchup(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("catchup error: %v", err)
		}
		return
	}

	log.Printf("starting client in %s mode, syncing to %s", *mode, absDir)

	if err := app.Client.Sync(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("sync error: %v", err)
	}
}

// --- pin / unpin ---------------------------------------------------------

func cmdPin(args []string, target hubsync.TargetState) {
	verb := "pin"
	if target == hubsync.TargetUnpinned {
		verb = "unpin"
	}
	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	dry := fs.Bool("dry", false, "print the plan; don't mutate anything")
	dir := fs.String("dir", ".", "hub directory (searches up for .hubsync)")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "usage: hubsync %s <glob>... [--dry]\n", verb)
		os.Exit(1)
	}

	hubDir, err := locateHubDir(*dir)
	if err != nil {
		log.Fatal(err)
	}

	req := hubsync.PinRequest{Globs: fs.Args(), Dry: *dry}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := runPinOrUnpin(ctx, hubDir, req, target)
	if err != nil {
		log.Fatalf("%s: %v", verb, err)
	}
	failures := printPinResults(verb, resp)
	if failures > 0 {
		os.Exit(1)
	}
}

// runPinOrUnpin takes the try-lock-then-RPC fork in the road:
//   - acquired → run in-process (scan first so new files are tracked).
//   - lock held + serve.sock reachable → RPC.
//   - lock held + no serve → clear error.
func runPinOrUnpin(ctx context.Context, hubDir string, req hubsync.PinRequest, target hubsync.TargetState) (*hubsync.PinResponse, error) {
	stack, release, err := acquireOneShot(hubDir, true)
	if err == nil {
		defer release()
		if scanErr := stack.hub.FullScan(); scanErr != nil {
			return nil, fmt.Errorf("scan: %w", scanErr)
		}
		resp, rerr := hubsync.RunReconcile(ctx, stack.reconciler, stack.store, req, target)
		if rerr != nil {
			return nil, rerr
		}
		return &resp, nil
	}
	if !errors.Is(err, hubsync.ErrLocked) {
		// Wiring failure (missing [archive], bad config, DB open error) —
		// don't mask it by falling through to RPC.
		return nil, err
	}
	// Lock held. Try RPC.
	if !rpcReachable(hubDir) {
		return nil, lockHeldMessage(hubDir, nil)
	}
	c := hubsync.NewRPCClient(hubsync.RPCSocketPath(hubDir), os.Getenv("HUBSYNC_TOKEN"))
	if target == hubsync.TargetArchived {
		return c.Pin(ctx, req)
	}
	return c.Unpin(ctx, req)
}

func printPinResults(verb string, resp *hubsync.PinResponse) int {
	failures := 0
	for _, r := range resp.Results {
		steps := strings.Join(r.Steps, " → ")
		prefix := verb
		if r.Dry {
			prefix = verb + " (dry)"
		}
		if r.Error != "" {
			fmt.Printf("%s %s [%s]: FAIL %s — %s\n", prefix, r.Path, r.StartingState, steps, r.Error)
			failures++
		} else {
			fmt.Printf("%s %s [%s]: %s\n", prefix, r.Path, r.StartingState, steps)
		}
	}
	return failures
}

// --- ls ------------------------------------------------------------------

// cmdLs follows the shared `ls` / `duckql` convention: no bespoke filter
// flags (pipe to duckql instead), TTY → human table, pipe → JSONL.
// Talks to a running serve via RPC when the socket is reachable; otherwise
// opens the hub DB read-only in-process (SQLite WAL permits this alongside
// a writer, so it's safe even if serve crashed mid-run).
func cmdLs(args []string) {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	dir := fs.String("dir", ".", "hub directory (searches up for .hubsync)")
	quiet := fs.Bool("quiet", false, "suppress warnings on stderr (no effect today)")
	fs.Parse(args)
	_ = quiet // convention placeholder — no warnings emitted yet

	hubDir, err := locateHubDir(*dir)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := fetchLs(ctx, hubDir)
	if err != nil {
		log.Fatalf("ls: %v", err)
	}

	renderLs(os.Stdout, resp.Entries)
}

// --- status --------------------------------------------------------------

// cmdStatus prints hub tree + archive counts. Talks to a running serve
// via RPC when reachable; otherwise reads the hub DB directly.
func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	dir := fs.String("dir", ".", "hub directory (searches up for .hubsync)")
	fs.Parse(args)

	hubDir, err := locateHubDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := fetchStatus(ctx, hubDir)
	if err != nil {
		log.Fatalf("status: %v", err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(s)
		return
	}
	fmt.Printf("tree       %6d files   %6d dirs\n", s.Tree.Files, s.Tree.Dirs)
	fmt.Printf("archived   %6d files   %10d bytes\n", s.Archive.Count, s.Archive.Bytes)
	fmt.Printf("dirty      %6d files   %10d bytes\n", s.Dirty.Count, s.Dirty.Bytes)
	fmt.Printf("unpinned   %6d files   %10d bytes\n", s.Unpinned.Count, s.Unpinned.Bytes)
	fmt.Printf("null       %6d files   %10d bytes\n", s.Null.Count, s.Null.Bytes)
}

// --- helpers -------------------------------------------------------------

// locateHubDir walks up from start looking for a directory containing
// .hubsync/. Returns the first match, or start itself if .hubsync exists
// there.
func locateHubDir(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	dir := abs
	for {
		if st, err := os.Stat(filepath.Join(dir, ".hubsync")); err == nil && st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .hubsync directory found at or above %s", abs)
		}
		dir = parent
	}
}
