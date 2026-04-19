package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/hayeah/hubsync"
	"github.com/hayeah/hubsync/archive"
)

// oneShotStack bundles the hand-wired hub components used by `archive` and
// the in-process `pin`/`unpin` path. Same dependencies as the HubApp, minus
// the Watcher, HTTP Server and RPC socket (none are needed for a one-shot
// run). Cleanup releases both the DB and the archive storage in reverse
// construction order.
type oneShotStack struct {
	hubDir     string
	config     *hubsync.ConfigFile
	store      *hubsync.HubStore
	hasher     hubsync.Hasher
	hub        *hubsync.Hub
	storage    archive.ArchiveStorage
	reconciler *hubsync.Reconciler

	cleanups []func()
}

// Release runs the registered cleanups in reverse order.
func (s *oneShotStack) Release() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
	s.cleanups = nil
}

// newOneShotStack builds the stack under hubDir. It always opens the DB,
// runs the scanner, and constructs the reconciler. When needArchive is
// true, it also opens the B2 storage (requires [archive] in config.toml);
// when false, Reconciler and ArchiveWorker are nil (sufficient for `ls`
// / `status` callers that don't need remote state).
func newOneShotStack(hubDir string, needArchive bool) (*oneShotStack, error) {
	cfg, err := hubsync.LoadConfigFile(hubDir)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if needArchive && cfg.Archive == nil {
		return nil, fmt.Errorf("archive not configured — add an [archive] section to %s", filepath.Join(hubDir, ".hubsync", "config.toml"))
	}

	hasher, err := hubsync.NewHasher(cfg.Hub.Hash)
	if err != nil {
		return nil, err
	}

	dbPath := hubsync.HubDBPath(hubDir)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	store, storeCleanup, err := hubsync.NewHubStore(hubsync.HubStoreConfig{DBPath: dbPath, Hasher: hasher})
	if err != nil {
		return nil, err
	}

	ignorer, err := hubsync.ProvideIgnorer(hubDir)
	if err != nil {
		storeCleanup()
		return nil, err
	}
	scanner := hubsync.NewScanner(hubsync.ScannerConfig{WatchDir: hubDir, Ignorer: ignorer, Hasher: hasher})
	broadcaster := hubsync.NewBroadcaster()
	// Watcher is nil — FullScan doesn't touch it.
	hub := hubsync.NewHub(store, scanner, nil, broadcaster, hasher)

	s := &oneShotStack{
		hubDir:   hubDir,
		config:   cfg,
		store:    store,
		hasher:   hasher,
		hub:      hub,
		cleanups: []func(){storeCleanup},
	}

	if needArchive {
		storage, storageCleanup, err := hubsync.ProvideArchiveStorage(cfg)
		if err != nil {
			s.Release()
			return nil, fmt.Errorf("open archive storage: %w", err)
		}
		s.storage = storage
		s.cleanups = append(s.cleanups, storageCleanup)

		prefix := cfg.Archive.BucketPrefix
		s.reconciler = &hubsync.Reconciler{
			Store:   store,
			Storage: storage,
			Hasher:  hasher,
			HubDir:  hubDir,
			Prefix:  prefix,
		}
	}
	return s, nil
}

// acquireOneShot resolves hubDir, takes the HubLock, and returns a ready-to-
// use oneShotStack plus a release func that drops both the stack and the
// lock. Used by archive/pin/unpin's in-process path.
func acquireOneShot(hubDir string, needArchive bool) (*oneShotStack, func(), error) {
	lock, err := hubsync.AcquireHubLock(hubDir)
	if err != nil {
		return nil, nil, err
	}
	stack, err := newOneShotStack(hubDir, needArchive)
	if err != nil {
		lock.Release()
		return nil, nil, err
	}
	return stack, func() {
		stack.Release()
		lock.Release()
	}, nil
}

// runCtx returns a context that cancels on SIGINT/SIGTERM. Caller defers
// the cancel.
func runCtx() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// rpcTimeout is the default timeout for one-shot RPCs.
const rpcTimeout = 30 * time.Second

// withTimeout is a tiny helper used by CLI commands.
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// fatalf logs and exits with code 2 (startup/wiring failures).
func fatalf(format string, args ...any) {
	log.Printf(format, args...)
	os.Exit(2)
}
