---
name: hubsync-cli
description: HubSync command-line tool. Use to run the hub server (watches a directory, serves changes) or run a client (read-only replica or read-write sync). Two subcommands - serve and client.
---

# hubsync CLI

Command-line entry point for HubSync. Two subcommands:

- `hubsync serve` — run the hub server
- `hubsync client` — run a sync client (read-only or read-write)

## Build

```bash
# From the repo root
go build -o cli/hubsync/hubsync ./cli/hubsync/

# Or install globally
go install ./cli/hubsync/
```

The binary uses `google/wire` for compile-time dependency injection. `wire_gen.go` is generated from `wire.go`:

```bash
# Regenerate wire (rarely needed)
go run -mod=mod github.com/google/wire/cmd/wire ./cli/hubsync/
```

## `hubsync serve`

Start the hub server. Watches a directory, maintains a versioned change log, and streams changes to subscribed clients.

```bash
# Default: watch current dir, listen on 127.0.0.1:8080
hubsync serve

# Watch a specific directory
hubsync serve -dir /path/to/files

# Listen on a different address (e.g. all interfaces)
hubsync serve -dir /path/to/files -listen 0.0.0.0:8080

# With auth (set HUBSYNC_TOKEN — both hub and clients need it)
HUBSYNC_TOKEN=mysecret hubsync serve -dir /path/to/files

# Custom DB path (default is <dir>/.hubsync/hub.db)
hubsync serve -dir /path/to/files -db /var/lib/hubsync/hub.db
```

| Flag | Default | Description |
|---|---|---|
| `-dir` | `.` | Directory to watch |
| `-listen` | `127.0.0.1:8080` | HTTP listen address |
| `-db` | `<dir>/.hubsync/hub.db` | SQLite database path |

The hub does an initial full scan on startup, then watches for filesystem changes via `fsnotify` (50ms debounce). Respects `.gitignore` patterns. Always excludes `.hubsync/` (its own state directory).

Graceful shutdown on SIGINT/SIGTERM.

## `hubsync client`

Run a sync client. Two modes:

- **Read mode** (default) — replica that mirrors the hub. Receives changes via the sync stream and writes them to the local directory.
- **Write mode** — replica that also pushes local changes back to the hub. Periodically scans the local directory, diffs against `hub_tree`, and pushes changes via `POST /push`.

```bash
# Read-only replica
hubsync client -hub http://localhost:8080 -dir /local/replica

# Read-write client (pushes local edits back)
hubsync client -hub http://localhost:8080 -dir /local/replica -mode write

# Faster scan interval for write mode
hubsync client -hub http://localhost:8080 -dir /local/replica -mode write -scan-interval 1s

# With auth
HUBSYNC_TOKEN=mysecret hubsync client -hub http://localhost:8080 -dir /local/replica
```

| Flag | Default | Description |
|---|---|---|
| `-hub` | (required) | Hub server URL, e.g. `http://localhost:8080` |
| `-dir` | `.` | Local directory to sync into |
| `-db` | `<dir>/.hubsync/client.db` | SQLite database path |
| `-mode` | `read` | Sync mode: `read` or `write` |
| `-scan-interval` | `5s` | Scan interval for write mode (Go duration string) |
| `-once` | `false` | Bootstrap and/or catch up to the hub's current state, then exit. Read mode only |

### First-connect bootstrap

On first run (empty client DB), the client:

1. Fetches the tree DB from `/snapshots-tree/latest` — gets all metadata in one request
2. Fetches each unique blob via `/blobs/{digest}` — deduplicated by digest
3. Connects to `/sync/subscribe?since={version}` for incremental updates

This is HTTP/2-friendly (parallel blob fetches) and CDN-cacheable.

### One-shot mode (`-once`)

`hubsync client -once` brings the local directory in sync with the hub's current state and exits. Idempotent — safe to re-run.

```bash
# One-shot sync (e.g. for cron, CI, or scripts)
hubsync client -hub http://localhost:8090 -dir /local/replica -once
```

The `-once` flow:

1. Fetches the latest tree DB from `/snapshots-tree/latest`
2. For each file in the tree: if local content already matches the hub's digest, skip; otherwise fetch via `/blobs/{digest}`
3. Walks the local directory and removes any files not in the hub tree (respects `.gitignore`, skips `.hubsync/`)
4. Exits

Logs a summary like `catchup complete, version=42, fetched=3, skipped=17, deleted=1`.

Use this for cron jobs, CI pipelines, or any context where you want a sync run with a definite end. For continuous sync, omit `-once`.

`-once` requires `-mode read`. Combining with `-mode write` is rejected.

### Write mode behavior

In write mode, the client runs two loops in parallel:

- **Sync stream loop** — receives changes from the hub, writes them to disk
- **Push loop** — every `-scan-interval`, scans the local directory, diffs against `hub_tree`, pushes changes to `POST /push`

On push conflict (someone else updated the file on the hub), the client:

- Renames the local file to `<name> (conflicted copy).<ext>`
- Fetches the hub's current version
- Logs the conflict for the caller to resolve

The client tracks "pending push paths" so the sync stream doesn't overwrite files with local edits between scan cycles.

## Environment Variables

| Variable | Description |
|---|---|
| `HUBSYNC_TOKEN` | Bearer token for auth. Set on both hub and client. If unset on the hub, no authentication is required (open). |

## Quirks & Conventions

- **DB path defaults** to `<dir>/.hubsync/hub.db` (serve) or `<dir>/.hubsync/client.db` (client). The `.hubsync/` directory is auto-created and always excluded from sync scans.
- **`-dir` is resolved to an absolute path** before use, so relative paths and symlinks are normalized.
- **No write mode for the hub** — the hub is the authoritative source. Pushes from clients go through `POST /push` with optimistic concurrency.
- **No `-token` flag** — use `HUBSYNC_TOKEN` env var instead. This avoids tokens leaking into shell history and `ps` output.
- **Wire DI**: `wire.go` defines the providers, `wire_gen.go` is generated. Don't edit `wire_gen.go` by hand.
- **Hub binary doubles as test fixture**: the Rust client integration tests at `rust-client/tests/integration.rs` build this binary into `rust-client/testdata/hubsync-server`. If you change the hub, rebuild that test fixture too.

## Examples

```bash
# Local development: hub on a directory, client mirroring it elsewhere
mkdir -p /tmp/hub-src /tmp/client-mirror
echo "hello" > /tmp/hub-src/README.md

# Terminal 1: start hub
hubsync serve -dir /tmp/hub-src -listen 127.0.0.1:8090

# Terminal 2: read-only client
hubsync client -hub http://localhost:8090 -dir /tmp/client-mirror
# → /tmp/client-mirror/README.md appears, mirroring /tmp/hub-src

# Terminal 3: edit on hub side
echo "updated" > /tmp/hub-src/README.md
# → client mirror updates within ~50ms (fsnotify debounce + sync stream)
```

```bash
# Bidirectional with two write-mode clients (will conflict if both edit the same file)
hubsync serve -dir /tmp/hub-src -listen 127.0.0.1:8090 &
hubsync client -hub http://localhost:8090 -dir /tmp/replica-a -mode write &
hubsync client -hub http://localhost:8090 -dir /tmp/replica-b -mode write &

# Edit on replica-a → propagates to hub → propagates to replica-b
echo "from a" > /tmp/replica-a/file.txt
sleep 6  # wait for scan + push + sync
cat /tmp/replica-b/file.txt
# → "from a"
```

## HTTP API

The hub exposes these endpoints. All require `Authorization: Bearer <token>` if `HUBSYNC_TOKEN` is set on the hub. Request/response bodies are protobuf (length-prefixed for streams).

| Method | Path | Description |
|---|---|---|
| `GET` | `/sync/subscribe?since={version}` | Long-lived stream of `SyncEvent` messages, length-prefixed protobuf |
| `GET` | `/blobs/{digest}` | Fetch file content by SHA-256 hex digest. Content-addressed, CDN-cacheable |
| `POST` | `/blobs/delta` | Rsync-style delta transfer for large file updates |
| `GET` | `/snapshots-tree/latest` | 302 redirect to `/snapshots-tree/{version}` |
| `GET` | `/snapshots-tree/{version}` | Hub_tree SQLite DB at the given version. Used for bootstrap |
| `POST` | `/push` | Push local changes (write mode). Optimistic concurrency via `base_version` |

### Sync Stream Frame Format

```
[4 bytes: message length, big-endian][SyncEvent protobuf bytes]
[4 bytes: message length, big-endian][SyncEvent protobuf bytes]
...
```

### Key Protobuf Messages

```protobuf
message SyncEvent {
  uint64 version = 1;
  string path = 2;
  oneof event {
    FileChange change = 3;
    FileDelete delete = 4;
  }
}

message FileChange {
  EntryKind kind = 1;       // FILE, DIRECTORY, SYMLINK
  bytes digest = 2;         // SHA-256
  uint64 size = 3;
  uint32 mode = 4;
  int64 mtime = 5;
  bytes data = 6;           // inline blob if size <= 64KB, else empty
}

message PushRequest {
  repeated PushOp ops = 1;
  ConflictPolicy on_conflict = 2;  // FAIL, HUB_WINS, CLIENT_WINS
}

message PushOp {
  string path = 1;
  OpKind op = 2;            // OP_CREATE, OP_UPDATE, OP_DELETE
  uint64 base_version = 3;  // version this edit is based on
  bytes digest = 4;
  bytes data = 5;
}

message PushResult {
  string path = 1;
  oneof outcome {
    PushAccepted accepted = 2;
    PushConflict conflict = 3;
  }
}
```

Full proto: `hubsync.proto` at the repo root.

## Sync Workflow

### Hub side

```
1. Startup
   - Open hub.db (SQLite, WAL mode)
   - Rebuild materialized tree from change_log
   - Run initial full scan to catch changes that happened while down
   - Start fsnotify watcher (50ms debounce) on the watched directory
   - Start HTTP server

2. On filesystem change
   - fsnotify event fires → debounced → triggers full scan
   - Scan walks the directory (respects .gitignore, skips .hubsync/)
   - For each file: stat, check (mtime, size) cache, hash if needed
   - Diff against materialized tree → list of creates/updates/deletes
   - For each change: append to change_log (auto-incrementing version),
     update tree, broadcast to all subscribers
   - Creates/updates always emitted before deletes (enables move dedup on client)

3. On client subscribe (GET /sync/subscribe?since=N)
   - Subscribe to broadcaster first (to avoid gap between backfill and live)
   - Backfill: stream all change_log entries with version > N
   - Then stream live broadcaster events (deduplicated by version)

4. On client push (POST /push)
   - For each PushOp:
     - CREATE: if path doesn't exist, write to disk, append to change_log, broadcast
     - UPDATE: if base_version matches current, accept; else conflict
     - DELETE: if base_version matches current, remove; else conflict
   - Conflict policy:
     - FAIL: return PushConflict with current version + digest
     - HUB_WINS: drop the change silently, return PushConflict (info only)
     - CLIENT_WINS: force overwrite regardless of base_version
```

### Client side (read mode)

```
1. Bootstrap (only if hub_version == 0)
   - GET /snapshots-tree/latest → SQLite DB
   - Import into local hub_tree, set hub_version
   - For each unique digest in tree: GET /blobs/{digest}, write to local FS
   - (Deduplicates by digest — files with identical content fetched once)

2. Sync stream
   - GET /sync/subscribe?since={hub_version}
   - For each SyncEvent:
     - FileChange:
       1. If data inlined (small file ≤64KB) → write directly
       2. Else if matching digest exists at another local path → copy
       3. Else if local file exists at this path → fetch delta via /blobs/delta
       4. Else → fetch full blob via /blobs/{digest}
       Update hub_tree + sync_state in one transaction
     - FileDelete:
       Remove local file
       Update hub_tree + sync_state in one transaction

3. Reconnect
   - On connection drop, sleep 1s and retry from last persisted hub_version
```

### Client side (write mode)

In addition to the read-mode flows above, write mode runs a parallel push loop:

```
1. Push loop (every -scan-interval, default 5s)
   - Walk local FS (respects .gitignore, skips .hubsync/)
   - For each file: stat, check (mtime, size) against hub_tree (digest cache)
   - Hash if cache miss
   - Compare against hub_tree:
     - File present locally but not in hub_tree → CREATE op
     - File present in both with different digest → UPDATE op (base_version from hub_tree)
     - File present in hub_tree but not locally → DELETE op
   - Build PushRequest, POST /push
   - Handle PushResponse:
     - PushAccepted → update hub_tree with new version
     - PushConflict (FAIL policy):
       - Rename local file to "<name> (conflicted copy).<ext>"
       - Fetch hub's current version via /blobs/{digest}, write to original path
       - Update hub_tree

2. Pending push tracking
   - Before each push, the client computes "pending push paths" (paths with local edits)
   - The sync stream loop checks this set: if a hub event arrives for a pending path,
     skip the FS write but still update hub_tree (so the next push knows the new base_version)
   - Prevents race where the hub's version overwrites a local edit before the push fires
```

### Bootstrap callback (Rust client API)

The Rust FFI client exposes a separate bootstrap callback so UI clients can show progress:

```c
typedef void (*HubSyncBootstrapCallback)(uint64_t count, uint64_t total, void *ctx);
```

- `(0, N)` — tree DB import complete, N entries
- `(count, total)` — blob fetch progress (future, currently not fired by Rust client which is metadata-only)

The Go client doesn't expose this callback (it's a CLI, no UI to update).

## Source Files

```
main.go      # CLI entry point (cmdServe, cmdClient)
wire.go      # Wire DI provider functions (compile-time DI)
wire_gen.go  # Generated by wire — do not edit
```

The actual hub/client logic lives in the parent package `github.com/hayeah/hubsync`. This CLI is a thin shell over `HubApp` and `ClientApp`.
