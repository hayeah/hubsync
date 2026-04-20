package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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

// renderLs emits a `//`-comment docstring (title, columns, underlying-DB
// path, sample duckql queries) followed by one JSONL row per entry. TTY
// vs. pipe doesn't matter — human-readable tables come from piping to
// `duckql` per the shared `ls` / `duckql` convention, which strips the
// `//` lines transparently. Entry order is stable (sorted by path
// server-side). Each HubEntry serializes through its JSON tags.
func renderLs(w io.Writer, hubRoot string, entries []hubsync.HubEntry) {
	renderLsDocstring(w, hubRoot)
	enc := json.NewEncoder(w)
	for _, e := range entries {
		_ = enc.Encode(e)
	}
}

// renderLsDocstring emits the `//`-comment prelude that describes the
// HubEntry JSONL shape and names the underlying SQLite DB for deeper
// queries. Follows the ls-pattern convention (see
// ~/Dropbox/notes/2026-04-17/ls-pattern_claude.md — "JSONL Docstring
// Prelude"). Callers that want the body without the docstring can skip
// this function; `duckql` strips `//` lines on input, so the normal
// pipeline handles the prelude transparently. Users reaching for `jq`
// directly need a `grep -v '^//'` prefilter.
func renderLsDocstring(w io.Writer, hubRoot string) {
	absDB, err := filepath.Abs(hubsync.HubDBPath(hubRoot))
	if err != nil {
		// Fall back to the un-resolved path; losing absolute-ness is
		// cosmetic, not a correctness issue for the docstring.
		absDB = hubsync.HubDBPath(hubRoot)
	}
	fmt.Fprintln(w, "// hubsync ls — hub tree entries (files + archive state)")
	fmt.Fprintln(w, "// Columns:")
	fmt.Fprintln(w, "//   path                text    hub-relative path (first key)")
	fmt.Fprintln(w, "//   kind                text    file | directory")
	fmt.Fprintln(w, "//   digest              text    content hash (hex)")
	fmt.Fprintln(w, "//   size                int     bytes")
	fmt.Fprintln(w, "//   mode                int     unix mode bits")
	fmt.Fprintln(w, "//   mtime               int     unix seconds")
	fmt.Fprintln(w, "//   version             int     monotonic version per entry")
	fmt.Fprintln(w, "//   archive_state       text    pinned | archived | unpinned | null")
	fmt.Fprintln(w, "//   archive_file_id     text    remote handle (archived only)")
	fmt.Fprintln(w, "//   archive_sha1        text    remote content hash (archived only)")
	fmt.Fprintln(w, "//   archive_uploaded_at int     unix millis (archived only)")
	fmt.Fprintln(w, "//   updated_at          int     unix seconds (row version timestamp)")
	fmt.Fprintf(w, "// Underlying DB: %s  (SQLite, WAL-safe read alongside serve)\n", absDB)
	fmt.Fprintln(w, "// Deeper queries:")
	fmt.Fprintln(w, "//   hubsync ls | duckql \"WHERE archive_state='unpinned'\"")
	fmt.Fprintln(w, "//   hubsync ls | duckql \"SELECT archive_state, count(*) GROUP BY 1\"")
	fmt.Fprintf(w, "//   duckql -i 'sqlite:%s' \"FROM hub_entry WHERE archive_state='unpinned'\"\n", absDB)
	fmt.Fprintln(w, "// Note: `hubsync ls | jq ...` requires `grep -v '^//'` first; `duckql` strips automatically.")
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
