package hubsync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHubLock_Contention(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".hubsync"), 0755); err != nil {
		t.Fatal(err)
	}

	first, err := AcquireHubLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { first.Release() })

	if _, err := AcquireHubLock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire: err=%v want ErrLocked", err)
	}

	first.Release()

	// After release, a fresh acquire should succeed.
	third, err := AcquireHubLock(dir)
	if err != nil {
		t.Fatalf("post-release acquire: %v", err)
	}
	third.Release()
}

func TestHubLock_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	// Omit the .hubsync dir to verify AcquireHubLock creates it.
	l, err := AcquireHubLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer l.Release()
	if _, err := os.Stat(HubLockPath(dir)); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
}

func TestHubLock_ReleaseNil(t *testing.T) {
	var l *HubLock
	l.Release() // should not panic
}
