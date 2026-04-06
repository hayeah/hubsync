package hubsync

import "sync"

// Broadcaster fans out change entries to all subscribers.
type Broadcaster struct {
	mu   sync.RWMutex
	subs map[chan ChangeEntry]struct{}
}

// NewBroadcaster creates a Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan ChangeEntry]struct{})}
}

// Subscribe returns a buffered channel that receives change entries.
func (b *Broadcaster) Subscribe() chan ChangeEntry {
	ch := make(chan ChangeEntry, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel and closes it.
func (b *Broadcaster) Unsubscribe(ch chan ChangeEntry) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
	close(ch)
}

// Broadcast sends an entry to all subscribers. Non-blocking; slow subscribers drop events.
func (b *Broadcaster) Broadcast(entry ChangeEntry) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- entry:
		default:
		}
	}
}
