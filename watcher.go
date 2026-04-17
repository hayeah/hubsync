package hubsync

import (
	"context"
	"log"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches the hub directory for filesystem changes using fsnotify.
type Watcher struct {
	dir     string
	watcher *fsnotify.Watcher
}

// NewWatcher creates a Watcher for the given directory.
func NewWatcher(cfg WatcherConfig) (*Watcher, func(), error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	if err := w.Add(cfg.WatchDir); err != nil {
		w.Close()
		return nil, nil, err
	}
	watcher := &Watcher{dir: cfg.WatchDir, watcher: w}
	cleanup := func() { w.Close() }
	return watcher, cleanup, nil
}

// Run starts the watcher loop. It coalesces events with a debounce and calls
// onChanges with the list of changed paths. Blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context, onChanges func(paths []string)) error {
	const debounce = 50 * time.Millisecond

	var timer *time.Timer
	pending := make(map[string]struct{})

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()

		case event, ok := <-w.watcher.Events:
			if !ok {
				return nil
			}
			pending[event.Name] = struct{}{}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				paths := make([]string, 0, len(pending))
				for p := range pending {
					paths = append(paths, p)
				}
				pending = make(map[string]struct{})
				onChanges(paths)
			})

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("watcher error: %v", err)
		}
	}
}
