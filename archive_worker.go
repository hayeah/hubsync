package hubsync

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/hayeah/hubsync/archive"
)

// ArchiveWorker drives dirty → archived transitions. It picks up NULL-state
// and 'dirty' rows from hub_entry and uploads them to B2 via an
// archive.ArchiveStorage.
//
// The baseline walk and the watch-mode incremental path collapse into one
// Run loop: startup enqueues the existing NULL/dirty rows, subscribe feeds
// subsequent create/update events, an N-worker pool drains both.
type ArchiveWorker struct {
	Store       *HubStore
	Storage     archive.ArchiveStorage
	Hasher      Hasher
	Broadcaster *Broadcaster
	HubDir      string // absolute path; files resolved as HubDir/<path>
	Prefix      string // bucket_prefix; keys computed as Prefix + <path>
	Workers     int    // number of upload goroutines; 0 → default 4
}

// Run starts the worker loop. Blocks until ctx is cancelled.
func (w *ArchiveWorker) Run(ctx context.Context) error {
	queue, wg := w.startPool(ctx, func(path string, err error) {
		if err != nil && ctx.Err() == nil {
			log.Printf("archive: upload %s: %v", path, err)
		}
	})

	// Baseline: enqueue every pending row before starting the subscribe loop.
	if err := w.enqueuePending(ctx, queue); err != nil {
		close(queue)
		wg.Wait()
		return fmt.Errorf("archive baseline: %w", err)
	}

	// Watch: each create/update broadcast becomes an enqueue. We intentionally
	// enqueue before checking archive_state; uploadOne will short-circuit if
	// the row is no longer dirty by the time the worker gets there.
	var sub chan ChangeEntry
	if w.Broadcaster != nil {
		sub = w.Broadcaster.Subscribe()
		defer w.Broadcaster.Unsubscribe(sub)
	}

	for {
		select {
		case <-ctx.Done():
			close(queue)
			wg.Wait()
			return ctx.Err()
		case ev, ok := <-sub:
			if !ok {
				close(queue)
				wg.Wait()
				return nil
			}
			if ev.Op == OpDelete || ev.Kind != FileKindFile {
				continue
			}
			select {
			case queue <- ev.Path:
			case <-ctx.Done():
			}
		}
	}
}

// startPool launches the worker goroutines and returns the queue + WaitGroup
// for the caller to drive. done is invoked per upload (with its error, if
// any); the caller owns aggregation. Closing queue signals the workers to
// exit after draining.
func (w *ArchiveWorker) startPool(ctx context.Context, done func(path string, err error)) (chan string, *sync.WaitGroup) {
	workers := w.Workers
	if workers <= 0 {
		workers = 4
	}
	queue := make(chan string, 256)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case p, ok := <-queue:
					if !ok {
						return
					}
					err := w.uploadOne(ctx, p)
					if done != nil {
						done(p, err)
					}
				}
			}
		}()
	}
	return queue, &wg
}

func (w *ArchiveWorker) enqueuePending(ctx context.Context, queue chan<- string) error {
	rows, err := w.Store.PendingArchiveRows()
	if err != nil {
		return err
	}
	for _, r := range rows {
		select {
		case queue <- r.Path:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// uploadOne uploads a single file, verifies it, and flips the row to
// archived. Delegates to archiveOne so the watch path shares the
// taskrunner path's idempotency contract — including the head-match
// short-circuit that prevents B2 version stacking when a previous
// upload crashed before MarkArchived. The (any, error) skip metadata
// from archiveOne is discarded; only success/failure matters here.
func (w *ArchiveWorker) uploadOne(ctx context.Context, path string) error {
	_, err := archiveOne(ctx, ArchiveTaskDeps{
		Store:   w.Store,
		Storage: w.Storage,
		Hasher:  w.Hasher,
		HubDir:  w.HubDir,
		Prefix:  w.Prefix,
	}, path)
	return err
}
