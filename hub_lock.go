package hubsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked is returned by AcquireHubLock when another process already holds
// the exclusive lock on .hubsync/hub.lock.
var ErrLocked = errors.New("hub lock held by another process")

// HubLock is a POSIX flock(LOCK_EX|LOCK_NB) on .hubsync/hub.lock. Exactly one
// of `hubsync serve`, `hubsync archive`, or an in-process `pin`/`unpin` may
// hold it at a time. The lock is released when the process exits (including
// on crash), so stale-lock cleanup is unnecessary — if the file exists but
// no process owns it, flock acquires it immediately.
//
// Callers should `defer l.Release()` after a successful acquire.
type HubLock struct {
	file *os.File
}

// AcquireHubLock creates (or opens) .hubsync/hub.lock under hubDir and takes
// an exclusive non-blocking flock on it. Returns ErrLocked when another
// process holds it.
func AcquireHubLock(hubDir string) (*HubLock, error) {
	lockPath := HubLockPath(hubDir)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("create .hubsync: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	return &HubLock{file: f}, nil
}

// Release drops the flock and closes the file. Safe to call on a nil or
// already-released lock.
func (l *HubLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
