package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hayeah/hubsync"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "client":
		cmdClient(os.Args[2:])
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
  hubsync serve    [flags]               Run the hub (scanner + watcher + archive + RPC).
  hubsync client   [flags]               Run a replica (read or write mode).
  hubsync pin      <glob>... [--dry]     Ensure matched paths are archived (remote + local).
  hubsync unpin    <glob>... [--dry]     Ensure matched paths are unpinned (remote only).
  hubsync ls       [glob] [--long]       List every tree entry with archive state.
  hubsync status   [--json]              Tree + archive counts.

All verbs besides 'serve' / 'client' talk to a running 'hubsync serve' over
.hubsync/serve.sock.
`)
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

	c := hubsync.NewRPCClient(hubsync.RPCSocketPath(hubDir), os.Getenv("HUBSYNC_TOKEN"))
	req := hubsync.PinRequest{Globs: fs.Args(), Dry: *dry}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var resp *hubsync.PinResponse
	if target == hubsync.TargetArchived {
		resp, err = c.Pin(ctx, req)
	} else {
		resp, err = c.Unpin(ctx, req)
	}
	if err != nil {
		log.Fatalf("%s: %v", verb, err)
	}
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
	if failures > 0 {
		os.Exit(1)
	}
}

// --- ls ------------------------------------------------------------------

func cmdLs(args []string) {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	long := fs.Bool("long", false, "show B2 fileId prefix")
	dir := fs.String("dir", ".", "hub directory (searches up for .hubsync)")
	fs.Parse(args)

	glob := ""
	if fs.NArg() > 0 {
		glob = fs.Arg(0)
	}

	hubDir, err := locateHubDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	c := hubsync.NewRPCClient(hubsync.RPCSocketPath(hubDir), os.Getenv("HUBSYNC_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Ls(ctx, glob)
	if err != nil {
		log.Fatalf("ls: %v", err)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, e := range resp.Entries {
		flag := archiveStateFlag(e.ArchiveState)
		if *long {
			fid := e.ArchiveFileID
			if len(fid) > 12 {
				fid = fid[:12] + "…"
			}
			fmt.Fprintf(tw, "%s\t%10d\t%s\t%s\n", flag, e.Size, fid, e.Path)
		} else {
			fmt.Fprintf(tw, "%s\t%s\n", flag, e.Path)
		}
	}
	tw.Flush()
}

func archiveStateFlag(s string) string {
	switch s {
	case "archived":
		return "a"
	case "unpinned":
		return "u"
	case "dirty":
		return "d"
	default:
		return "-"
	}
}

// --- status --------------------------------------------------------------

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	dir := fs.String("dir", ".", "hub directory (searches up for .hubsync)")
	fs.Parse(args)

	hubDir, err := locateHubDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	c := hubsync.NewRPCClient(hubsync.RPCSocketPath(hubDir), os.Getenv("HUBSYNC_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := c.Status(ctx)
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
