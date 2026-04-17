package hubsync

import (
	"context"
	"fmt"
	"log"
)

// Hub orchestrates the hub-side components: store, scanner, watcher, broadcaster.
type Hub struct {
	Store       *HubStore
	Scanner     *Scanner
	Watcher     *Watcher
	Broadcaster *Broadcaster
	Hasher      Hasher
}

// NewHub creates a Hub.
func NewHub(store *HubStore, scanner *Scanner, watcher *Watcher, bc *Broadcaster, hasher Hasher) *Hub {
	return &Hub{
		Store:       store,
		Scanner:     scanner,
		Watcher:     watcher,
		Broadcaster: bc,
		Hasher:      hasher,
	}
}

// Start performs an initial full scan and then starts the watcher loop.
// It blocks until ctx is cancelled.
func (h *Hub) Start(ctx context.Context) error {
	if err := h.FullScan(); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}
	log.Printf("initial scan complete, version=%d", h.mustLatestVersion())

	if h.Watcher != nil {
		return h.Watcher.Run(ctx, func(paths []string) {
			if err := h.FullScan(); err != nil {
				log.Printf("scan error: %v", err)
			}
		})
	}

	<-ctx.Done()
	return ctx.Err()
}

// FullScan scans the directory and appends any changes to the store.
func (h *Hub) FullScan() error {
	tree := h.Store.TreeSnapshot()
	scanned, err := h.Scanner.ScanAll(tree)
	if err != nil {
		return err
	}

	changes := h.Scanner.Diff(scanned, tree)
	for _, c := range changes {
		version, err := h.Store.Append(c)
		if err != nil {
			return fmt.Errorf("append change for %s: %w", c.Path, err)
		}
		c.Version = version
		h.Broadcaster.Broadcast(c)
	}
	return nil
}

func (h *Hub) mustLatestVersion() int64 {
	v, _ := h.Store.LatestVersion()
	return v
}
