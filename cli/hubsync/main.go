package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hayeah/hubsync"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: hubsync <serve|client>\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "client":
		cmdClient(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

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

	// Start hub in background
	go func() {
		if err := app.Hub.Start(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("hub error: %v", err)
		}
	}()

	// Start HTTP server
	go func() {
		if err := app.Server.ListenAndServe(); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
}

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
