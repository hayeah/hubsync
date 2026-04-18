package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hayeah/hubsync"
)

// fetchStatus tries the running serve via RPC, falling back to a
// read-only DB open when the socket isn't reachable.
func fetchStatus(ctx context.Context, hubDir string) (*hubsync.StatusResponse, error) {
	if rpcReachable(hubDir) {
		c := hubsync.NewRPCClient(hubsync.RPCSocketPath(hubDir), os.Getenv("HUBSYNC_TOKEN"))
		return c.Status(ctx)
	}
	store, cleanup, err := openHubStoreRO(hubDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	resp, err := hubsync.LocalStatus(store)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// renderLs emits one JSONL row per entry. TTY vs. pipe doesn't matter —
// human-readable tables come from piping to `duckql` per the shared
// `ls` / `duckql` convention. Entry order is stable (sorted by path
// server-side). Each HubEntry serializes through its JSON tags — no
// projection struct needed since FileKind and Digest carry their own
// MarshalJSON.
func renderLs(w io.Writer, entries []hubsync.HubEntry) {
	enc := json.NewEncoder(w)
	for _, e := range entries {
		_ = enc.Encode(e)
	}
}

// rpcReachable reports whether a hubsync serve is listening on the hub's
// unix socket. Fast: one connect attempt with a short timeout.
func rpcReachable(hubDir string) bool {
	sock := hubsync.RPCSocketPath(hubDir)
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// openHubStoreRO opens .hubsync/hub.db read-only. Skips the ensureHashAlgo
// write that NewHubStore normally performs.
func openHubStoreRO(hubDir string) (*hubsync.HubStore, func(), error) {
	dbPath := hubsync.HubDBPath(hubDir)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil, fmt.Errorf("hub db not found at %s (has the hub ever been scanned?)", dbPath)
	}
	db, err := hubsync.OpenDBReadOnly(dbPath)
	if err != nil {
		return nil, nil, err
	}
	store := &hubsync.HubStore{DB: db}
	return store, func() { db.Close() }, nil
}

// lockHeldMessage formats the "lock held, no serve reachable" error text
// shared by pin/unpin/archive.
func lockHeldMessage(hubDir string, cause error) error {
	path := hubsync.HubLockPath(hubDir)
	suffix := ""
	if cause != nil && !errors.Is(cause, hubsync.ErrLocked) {
		suffix = " (" + cause.Error() + ")"
	}
	return fmt.Errorf("hub at %s is locked by another hubsync process (%s)%s; wait for it to finish, or remove the lock file if it's stale", hubDir, path, suffix)
}

// formatBytes renders a human-friendly byte count (best-effort, no flourishes).
func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n) / 1024
	for _, u := range units {
		if f < 1024 {
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".") + " " + u
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f PB", f)
}
