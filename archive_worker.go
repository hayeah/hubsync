package hubsync

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// ArchiveUpload is one successful upload result reported by RunOnce.
type ArchiveUpload struct {
	Path string
	Size int64
}

// ArchiveFailure is one failed upload reported by RunOnce. Err is preserved
// verbatim — callers format it for stderr.
type ArchiveFailure struct {
	Path string
	Err  error
}

// ArchiveResult aggregates what a RunOnce pass did.
type ArchiveResult struct {
	Uploaded []ArchiveUpload
	Failed   []ArchiveFailure
}

// TotalBytes returns the sum of Uploaded[].Size (useful for the CLI summary).
func (r ArchiveResult) TotalBytes() int64 {
	var n int64
	for _, u := range r.Uploaded {
		n += u.Size
	}
	return n
}

// RunOnce is the single-pass variant of Run used by `hubsync archive`.
// Enqueues every row returned by PendingArchiveRows, drains the worker pool,
// and returns when the queue is empty. Unlike Run it does not subscribe to
// the broadcaster — one scan's worth of rows is the full batch.
//
// onProgress, if non-nil, is called from the worker goroutines as each
// upload completes (err == nil on success). Calls are serialized so
// callers can write to stderr without their own mutex.
func (w *ArchiveWorker) RunOnce(ctx context.Context, onProgress func(ArchiveUpload, error)) (ArchiveResult, error) {
	rows, err := w.Store.PendingArchiveRows()
	if err != nil {
		return ArchiveResult{}, fmt.Errorf("archive pending rows: %w", err)
	}

	var (
		mu     sync.Mutex
		result ArchiveResult
	)

	queue, wg := w.startPool(ctx, func(path string, upErr error) {
		// Look up the row again to get the size for the upload record.
		var size int64
		if e, ok, _ := w.Store.EntryLookup(path); ok {
			size = e.Size
		}
		mu.Lock()
		if upErr != nil {
			result.Failed = append(result.Failed, ArchiveFailure{Path: path, Err: upErr})
		} else {
			result.Uploaded = append(result.Uploaded, ArchiveUpload{Path: path, Size: size})
		}
		mu.Unlock()
		if onProgress != nil {
			if upErr != nil {
				onProgress(ArchiveUpload{Path: path, Size: size}, upErr)
			} else {
				onProgress(ArchiveUpload{Path: path, Size: size}, nil)
			}
		}
	})

	for _, r := range rows {
		select {
		case queue <- r.Path:
		case <-ctx.Done():
			close(queue)
			wg.Wait()
			return result, ctx.Err()
		}
	}
	close(queue)
	wg.Wait()

	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
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

// uploadOne uploads a single file, verifies it, and flips the row to archived.
// Idempotent: if the row is no longer dirty (e.g. someone else archived it
// already) the function returns without error.
func (w *ArchiveWorker) uploadOne(ctx context.Context, path string) error {
	entry, ok, err := w.Store.EntryLookup(path)
	if err != nil {
		return err
	}
	if !ok || entry.Kind != FileKindFile {
		return nil
	}
	switch entry.ArchiveState {
	case ArchiveStateArchived, ArchiveStateUnpinned:
		return nil
	}

	fullPath := filepath.Join(w.HubDir, path)
	src, err := os.Open(fullPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", fullPath, err)
	}
	defer src.Close()

	// blazer computes B2's content SHA-1 internally (single-part: buffered
	// hash; large-file: per-part + large_file_sha1). We trust entry.Digest
	// as the scanner-computed content hash for hubsync_digest; the in-place-
	// rewrite mitigation (stream-hash during upload and verify) lands later.
	key := w.Prefix + path
	info, err := w.Storage.Upload(ctx, archive.UploadRequest{
		Key:        key,
		Size:       entry.Size,
		Source:     src,
		Digest:     entry.Digest.Bytes(),
		DigestAlgo: w.Hasher.Name(),
		MTime:      time.Unix(entry.MTime, 0),
	})
	if err != nil {
		return err
	}

	var sha1Bytes []byte
	if info.ContentSHA1 != "" {
		if b, err := hex.DecodeString(info.ContentSHA1); err == nil {
			sha1Bytes = b
		}
	}
	return w.Store.MarkArchived(path, info.FileID, sha1Bytes, info.UploadedAt.UnixMilli())
}
