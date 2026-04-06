---
name: hubsync
description: Hub-and-client file synchronization system with versioned changelog, delta transfer, and read-write push support. Use when building or operating file sync between a hub directory and remote replicas.
---

# HubSync

File synchronization with a hub-and-client architecture. The hub watches a directory and streams changes to connected clients. Clients can be read-only replicas or read-write (push local changes back).

Files are opaque blobs — no content-level merging. Conflicts produce a "conflicted copy" that agents or users resolve per-file.

## Quick Start

```bash
# Build
cd cli/hubsync && go build -o hubsync .

# Start hub (watches current directory)
HUBSYNC_TOKEN=secret ./hubsync serve -dir /path/to/files -listen 127.0.0.1:8080

# Start read-only client
HUBSYNC_TOKEN=secret ./hubsync client -hub http://localhost:8080 -dir /path/to/replica

# Start read-write client (pushes local changes back)
HUBSYNC_TOKEN=secret ./hubsync client -hub http://localhost:8080 -dir /path/to/replica -mode write
```

## CLI

### `hubsync serve`

Start the hub server.

| Flag | Default | Description |
|---|---|---|
| `-dir` | `.` | Directory to watch |
| `-listen` | `127.0.0.1:8080` | HTTP listen address |
| `-db` | `<dir>/.hubsync/hub.db` | SQLite database path |

### `hubsync client`

Start a sync client.

| Flag | Default | Description |
|---|---|---|
| `-hub` | (required) | Hub URL, e.g. `http://localhost:8080` |
| `-dir` | `.` | Local sync directory |
| `-db` | `<dir>/.hubsync/client.db` | SQLite database path |
| `-mode` | `read` | `read` or `write` |
| `-scan-interval` | `5s` | How often to scan for local changes (write mode) |

### Environment Variables

| Variable | Description |
|---|---|
| `HUBSYNC_TOKEN` | Bearer token for auth. Set on both hub and client. If unset on hub, no auth required. |

## Architecture

```
Hub Server                        Client(s)
+- HubStore (SQLite change_log)   +- ClientStore (SQLite hub_tree)
+- Scanner (lstree, .gitignore)   +- Sync stream consumer
+- Watcher (fsnotify, 50ms)       +- Push loop (write mode)
+- Broadcaster (channels)         +- Delta engine (rsync)
+- HTTP Server                    +- Conflicted copy handler
```

## Protocol

All endpoints require `Authorization: Bearer <token>` when `HUBSYNC_TOKEN` is set. Streaming uses length-prefixed protobuf (4-byte big-endian size + protobuf bytes).

### Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/sync/subscribe?since={version}` | Stream changes as `SyncEvent` messages |
| GET | `/blobs/{digest}` | Fetch file content by SHA-256 hex digest |
| POST | `/blobs/delta` | Delta transfer (rsync-style) |
| GET | `/snapshots-tree/latest` | Redirect to latest tree-only snapshot |
| GET | `/snapshots-tree/{version}` | Download hub_tree SQLite DB (no file blobs) |
| POST | `/push` | Push local changes (write mode) |

### Sync Stream

Client connects to `/sync/subscribe?since=N`. Server backfills all changes after version N, then streams live events.

Each `SyncEvent` contains a monotonic version, file path, and either a `FileChange` (with metadata + optional inline data) or `FileDelete`.

- Files <=64KB: content inlined in the event
- Files >64KB: client fetches via `/blobs/{digest}` or delta transfer

### Push (Write Mode)

Client sends `PushRequest` with a list of `PushOp` entries. Each op carries a `base_version` from the client's `hub_tree` for optimistic concurrency.

- **Version matches**: accepted, written to hub disk, broadcast to all clients
- **Version stale**: conflict returned with current version and digest

Conflict policies:

| Policy | Behavior | Use case |
|---|---|---|
| `FAIL` | Return conflict info, client creates conflicted copy | Default — agents reconcile per-file |
| `HUB_WINS` | Silently drop conflicting op | Fire-and-forget agents |
| `CLIENT_WINS` | Force overwrite hub | Trusted editors |

### Conflicted Copy

On conflict with `FAIL` policy, the client:

- Renames local file to `<name> (conflicted copy).<ext>`
- Fetches the hub's current version to the original path
- If a conflicted copy already exists, appends a number: `(conflicted copy 2)`

### Delta Transfer

For large file updates, the client sends block signatures (rolling hash + SHA-256) and the hub returns delta operations (copy existing block vs. new data). Same algorithm as rsync.

Block size heuristic: `sqrt(24 * fileSize)`, clamped to [1KB, 64KB].

### Bootstrap

New clients bootstrap by fetching the tree DB and then each blob individually:

- `GET /snapshots-tree/latest` — download the hub_tree SQLite DB (file metadata + version cursor)
- Walk the tree, fetch each unique blob via `GET /blobs/{digest}` (deduplicated by digest)
- Connect to sync stream from the snapshot version for incremental updates

This is HTTP/2 friendly (parallel blob fetches over one connection) and CDN-cacheable (`/blobs/{digest}` is content-addressed and infinitely cacheable).

For metadata-only clients (e.g. the Rust client which is SQLite-only, no FS writes), just the tree DB fetch is sufficient — skip blob fetching entirely.

## Database Schema

### Hub: `change_log` (append-only)

```sql
CREATE TABLE change_log (
  version   INTEGER PRIMARY KEY AUTOINCREMENT,
  path      TEXT NOT NULL,
  op        TEXT NOT NULL CHECK(op IN ('create', 'update', 'delete')),
  kind      INTEGER NOT NULL DEFAULT 0,
  digest    TEXT,
  size      INTEGER,
  mode      INTEGER,
  mtime     INTEGER NOT NULL
);
```

### Client: `hub_tree` (materialized state)

```sql
CREATE TABLE hub_tree (
  path    TEXT PRIMARY KEY,
  kind    INTEGER NOT NULL DEFAULT 0,
  digest  TEXT,
  size    INTEGER,
  mode    INTEGER,
  mtime   INTEGER,
  version INTEGER NOT NULL
);
```

The `version` column in `hub_tree` is the change_log version that last touched this entry — used as `base_version` when pushing.

## Key Implementation Details

- **SQLite**: Pure Go driver (`modernc.org/sqlite`), WAL mode, 5s busy timeout
- **Ignore patterns**: Respects `.gitignore` via `go-lstree`. Always excludes `.hubsync/`
- **Digest caching**: Scanner reuses existing digest if mtime+size unchanged
- **Deduplication**: Client checks `hub_tree` for matching digest at different path before fetching
- **Broadcast**: Channel-per-subscriber (buffered 256), slow subscribers drop events
- **Reconnect**: Client retries with 1-second backoff on connection loss
- **Wire**: Compile-time dependency injection for both hub and client

## File Structure

```
hubsync.proto           # Protobuf message definitions
hubsync.go              # Core types: Digest, ChangeEntry, TreeEntry, FileKind
hub.go                  # Hub orchestrator (scan + watch + broadcast)
client.go               # Client sync engine (read + write modes)
server.go               # HTTP API handler
store.go                # HubStore (change_log + materialized tree)
client_store.go         # ClientStore (hub_tree + sync_state)
push.go                 # Hub-side push processing
client_push.go          # Client-side local scanning, diff, push
scanner.go              # Directory scanner with digest caching
watcher.go              # Filesystem watcher (fsnotify, 50ms debounce)
broadcast.go            # Change broadcaster
snapshot.go             # Snapshot generation/extraction (zstd tarball)
delta.go                # Rsync-style delta computation
protocol.go             # Length-prefixed protobuf I/O
auth.go                 # Bearer token auth middleware
config.go               # HubConfig, ClientConfig
providers.go            # Wire dependency providers
cli/hubsync/main.go     # CLI entry point
docs/spec.md            # Full read-only protocol spec
docs/client-read-write-spec.md  # Write mode spec
```

## Testing

```bash
# Run all tests
go test -v -timeout 120s

# Run push/write mode tests only
go test -run TestE2EPush -v

# Run specific test
go test -run TestE2EWriteMode -v
```

E2E tests spin up an in-process hub + httptest server + client. No external dependencies needed.
