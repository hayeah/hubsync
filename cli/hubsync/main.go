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
  hubsync init     [dir]                     Scaffold .hubsync/config.toml under dir (default: .).
  hubsync serve    [flags]                   Run the hub (scanner + watcher + archive + RPC).
  hubsync client   [flags]                   Run a replica (read or write mode).
  hubsync archive  [--dry] [--quiet]         One-shot: scan + upload pending files to B2, then exit.
  hubsync archive-gc [path] [--dry]          List a B2 prefix (cwd-relative) and delete objects no hub_entry claims.
  hubsync pin      <glob>... [--dry]         Ensure matched paths are archived (remote + local).
  hubsync unpin    <glob>... [--dry]         Ensure matched paths are unpinned (remote only).
  hubsync ls       [path] [--all]            List hub tree entries at path (cwd-relative); --all dumps every row.
  hubsync status   [--json]                  Tree + archive counts.

All operational verbs walk up from cwd to find .hubsync/ — run from anywhere
inside a hub tree. Path args (ls, archive-gc) are cwd-relative, same as git.

'archive', 'pin', 'unpin', 'status' work with or without a running
'hubsync serve': if serve is up, they RPC to .hubsync/serve.sock; otherwise
they run in-process after taking an exclusive flock on .hubsync/hub.lock.
'ls' always reads hub.db directly (read-only) and never RPCs.
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
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	dbPath := fs.String("db", "", "database path (default: <hub>/.hubsync/hub.db)")
	fs.Parse(args)

	hc, err := hubContext()
	if err != nil {
		fatalf("%v", err)
	}

	if *dbPath == "" {
		*dbPath = hubsync.HubDBPath(hc.root)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	// Exclusive lock — blocks a second serve, or an `archive`/in-process
	// pin/unpin, from touching this hub while we run.
	lock, err := hubsync.AcquireHubLock(hc.root)
	if err != nil {
		log.Fatalf("acquire hub lock: %v", err)
	}
	defer lock.Release()

	app, cleanup, err := InitializeHubApp(&hubsync.HubConfig{
		DBPath:   *dbPath,
		WatchDir: hc.root,
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
	dbPath := fs.String("db", "", "database path (default: <hub>/.hubsync/client.db)")
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

	hc, err := hubContext()
	if err != nil {
		fatalf("%v", err)
	}

	if *dbPath == "" {
		*dbPath = filepath.Join(hc.root, ".hubsync", "client.db")
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	app, cleanup, err := InitializeClientApp(&hubsync.ClientConfig{
		DBPath:       *dbPath,
		HubURL:       *hubURL,
		Token:        os.Getenv("HUBSYNC_TOKEN"),
		SyncDir:      hc.root,
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
		log.Printf("catching up %s from %s", hc.root, *hubURL)
		if err := app.Client.Catchup(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("catchup error: %v", err)
		}
		return
	}

	log.Printf("starting client in %s mode, syncing to %s", *mode, hc.root)

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
	fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "usage: hubsync %s <glob>... [--dry]\n", verb)
		os.Exit(1)
	}

	hc, err := hubContext()
	if err != nil {
		fatalf("%v", err)
	}

	req := hubsync.PinRequest{Globs: fs.Args(), Dry: *dry}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := runPinOrUnpin(ctx, hc.root, req, target)
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
// flags (pipe to duckql instead), all output goes through the HubEntry
// JSONL shape. Always opens hub.db read-only — SQLite WAL permits a
// reader alongside a live writer, so this is safe even when serve is
// running. No RPC, no serve.sock probe.
//
// Grammar:
//
//	hubsync ls                        # list cwd's level in the hub
//	hubsync ls <path>                 # list <path> resolved relative to cwd
//	hubsync ls .                      # same as bare ls
//	hubsync ls --all                  # every hub_entry row (old behavior)
func cmdLs(args []string) {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	all := fs.Bool("all", false, "emit every hub_entry row (pre-prefix dump behavior)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hubsync ls [path] [--all]\n\nList hub tree entries at a cwd-relative path, collapsed at /.\n--all dumps every hub_entry row regardless of path.\n")
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
	prefix, err := hc.resolvePrefix(arg)
	if err != nil {
		fatalf("ls: %v", err)
	}

	store, cleanup, err := openHubStoreRO(hc.root)
	if err != nil {
		fatalf("ls: %v", err)
	}
	defer cleanup()

	resp, err := hubsync.LocalLs(store, hubsync.LsRequest{Prefix: prefix, All: *all})
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
	fs.Parse(args)

	hc, err := hubContext()
	if err != nil {
		fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := fetchStatus(ctx, hc.root)
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

// --- hub context / path resolution --------------------------------------

// hubCtx carries the hub root + the hub-relative cwd (empty at root,
// "sub/" one level down, etc.). Captured once at process start; used by
// every verb that needs to resolve cwd-relative path args.
type hubCtx struct {
	root   string // absolute path to the hub root
	relCwd string // "" at root, "sub/" one level down
}

// hubContext walks up from cwd looking for .hubsync/. Returns the hub
// root and the cwd's position within it. Errors with a single canonical
// message if no hub is found in cwd or any ancestor.
func hubContext() (hubCtx, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return hubCtx{}, err
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return hubCtx{}, err
	}
	dir := absCwd
	for {
		if st, statErr := os.Stat(filepath.Join(dir, ".hubsync")); statErr == nil && st.IsDir() {
			rel, relErr := filepath.Rel(dir, absCwd)
			if relErr != nil {
				return hubCtx{}, relErr
			}
			if rel == "." {
				rel = ""
			} else {
				rel = filepath.ToSlash(rel) + "/"
			}
			return hubCtx{root: dir, relCwd: rel}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return hubCtx{}, fmt.Errorf("no .hubsync/ found in cwd or any ancestor.")
		}
		dir = parent
	}
}

// resolvePrefix turns a cwd-relative CLI arg into a hub-relative dir-
// prefix ("" at root, "foo/bar/" otherwise). Paths are joined onto the
// hub-relative cwd, same as git's cwd-relative path semantics.
// Errors if the resolved path escapes the hub root.
func (c hubCtx) resolvePrefix(arg string) (string, error) {
	if arg == "" || arg == "." {
		return c.relCwd, nil
	}
	abs := filepath.Join(c.root, c.relCwd, arg)
	rel, err := filepath.Rel(c.root, abs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside the hub root (%s)", arg, c.root)
	}
	if rel == "." {
		return "", nil
	}
	return rel + "/", nil
}
