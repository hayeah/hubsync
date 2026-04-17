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
	"text/tabwriter"
	"time"

	"github.com/hayeah/hubsync"
)

// fetchLs tries the running serve via RPC, falling back to a read-only DB
// open when the socket isn't reachable. Either path produces the same
// LsResponse shape.
func fetchLs(ctx context.Context, hubDir string) (*hubsync.LsResponse, error) {
	if rpcReachable(hubDir) {
		c := hubsync.NewRPCClient(hubsync.RPCSocketPath(hubDir), os.Getenv("HUBSYNC_TOKEN"))
		return c.Ls(ctx)
	}
	store, cleanup, err := openHubStoreRO(hubDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	resp, err := hubsync.LocalLs(store)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// fetchStatus mirrors fetchLs for the status endpoint.
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

// renderLs follows the `ls` convention: TTY → human table, pipe → JSONL.
// Keeps entry order stable (already sorted by path on the server side).
func renderLs(w io.Writer, entries []hubsync.LsEntry) {
	if isTerminal(w) {
		renderLsTable(w, entries)
		return
	}
	renderLsJSONL(w, entries)
}

func renderLsJSONL(w io.Writer, entries []hubsync.LsEntry) {
	enc := json.NewEncoder(w)
	for _, e := range entries {
		_ = enc.Encode(e)
	}
}

func renderLsTable(w io.Writer, entries []hubsync.LsEntry) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tKIND\tSTATE\tSIZE\tMTIME\tDIGEST")
	for _, e := range entries {
		state := e.State
		if state == "" {
			state = "-"
		}
		digest := e.DigestHex
		if len(digest) > 12 {
			digest = digest[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			e.Path, e.Kind, state, e.Size, e.MTime, digest)
	}
	_ = tw.Flush()
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

// isTerminal reports whether w writes to a TTY. Takes io.Writer so tests can
// pass bytes.Buffer and deterministically get JSONL. Matches the convention
// in the ls-pattern note: "TTY auto-table" is the only auto-magic.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
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
