# Hub Sync: Read-Only Client Spec

A hub-to-client file synchronization system. The hub is authoritative; the client is a read-only replica. Files are opaque blobs — no content-level merging.

For write mode (clients pushing local changes back), see `client-read-write-spec.md`.

## Overview

- Hub watches a directory, maintains a versioned change log in SQLite
- Clients subscribe to a stream of changes and apply them locally
- First-connect bootstraps by fetching the tree DB (`/snapshots-tree/latest`) then individual blobs via `/blobs/{digest}` (HTTP/2 friendly, CDN-cacheable)
- Incremental sync via length-prefixed protobuf stream over HTTP
- Bearer token authentication via `HUBSYNC_TOKEN` env var

## Hub

### Versioning

Global monotonic counter. Every mutation (create, update, delete) bumps it.

```
version 1: create  /README.md        digest=aaa
version 2: create  /src/main.go      digest=bbb
version 3: update  /README.md        digest=ccc
version 4: delete  /old/thing.txt
```

Client tracks a single integer (`hub_version`). Catch-up is `since=N`.

### Hub Storage Schema

```sql
-- Append-only change log. Source of truth.
CREATE TABLE change_log (
  version   INTEGER PRIMARY KEY AUTOINCREMENT,
  path      TEXT NOT NULL,
  op        TEXT NOT NULL CHECK(op IN ('create', 'update', 'delete')),
  kind      INTEGER NOT NULL DEFAULT 0, -- 0=file, 1=directory, 2=symlink
  digest    TEXT,           -- hex-encoded SHA-256, NULL for deletes
  size      INTEGER,
  mode      INTEGER,
  mtime     INTEGER NOT NULL
);

CREATE INDEX idx_change_log_path ON change_log(path);
```

No `blobs` table — the hub serves files directly from the watched directory, looking up the file path by digest in the in-memory materialized tree.

The hub maintains an in-memory materialized tree (`map[string]TreeEntry`) rebuilt from `change_log` on startup. This tree is the source of truth for current file state and is used for:
- Generating snapshot DBs
- Detecting filesystem changes (diffing scan results)
- Serving blob requests (digest → path lookup)

### Filesystem Change Detection

The hub uses a full directory scan + fsnotify watcher:

```
scan():
  walk directory with lstree (respects .gitignore)
  for each file:
    stat(path) -> (mtime, size)
    current = current_tree.get(path)
    if current and (mtime, size) match -> skip (digest cache hit)
    hash file -> SHA-256 digest
    if current and digest == current.digest -> skip
    append to change_log, update current tree
    broadcast to connected subscribers

  for each path in current_tree not found on disk:
    append delete to change_log
    broadcast
```

On startup: rebuild tree from change_log, run a full scan to catch changes while down, then start fsnotify watcher.

The `.gitignore` filtering uses `go-lstree`'s `Ignorer` interface, which supports full gitignore patterns when `.gitignore` exists, and falls back to a builtin junk list (node_modules, \_\_pycache\_\_, .venv, .DS_Store, etc.) when it doesn't.

#### Event Ordering: Creates Before Deletes

When processing a batch of filesystem changes, the hub emits creates/updates before deletes in the change log. This ensures the client's dedup index works for moves:

- **Create-first:** client sees the new path, looks up digest in `hub_tree`, finds the old path still present, copies locally. Then the delete removes the old row.
- **Delete-first:** client removes the old `hub_tree` row, then sees the create with no dedup match — unnecessary full fetch.

#### Filesystem Watcher

Uses `fsnotify` with 50ms debounce. When events fire, a full scan is triggered on the changed paths. If the watcher overflows or errors, a full scan covers everything.

### Tree Snapshots

For first-connect and far-behind clients. A tree snapshot is just the materialized tree as a SQLite DB — no file blobs. Clients fetch blobs separately by digest.

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

CREATE INDEX idx_hub_tree_digest ON hub_tree(digest);

CREATE TABLE sync_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- row: ('hub_version', '500')
```

Tree snapshots are generated on-demand when a client requests one. The hub serializes its in-memory tree into a fresh SQLite DB and serves it as a single response. No tarball, no compression — SQLite is already compact.

This design is HTTP/2 friendly (clients can fetch many blobs in parallel over one connection) and CDN-cacheable (`/blobs/{digest}` is content-addressed and infinitely cacheable). Clients that only need metadata (e.g. the Rust client which is SQLite-only, no FS writes) skip the blob fetches entirely.

### Log Compaction

Not yet implemented. The hub could drop old change_log entries past a retention window.

---

## Protocol

HTTP + protobuf. Sync stream uses length-prefixed protobuf messages over chunked HTTP. Bearer token auth on all endpoints.

### Protobuf Definitions

```protobuf
syntax = "proto3";
package hubsync;

// --- Sync stream ---

message SyncEvent {
  uint64 version = 1;
  string path = 2;

  oneof event {
    FileChange change = 3;
    FileDelete delete = 4;
  }
}

message FileChange {
  EntryKind kind = 1;
  bytes digest = 2;
  uint64 size = 3;
  uint32 mode = 4;
  int64 mtime = 5;
  bytes data = 6;       // inline blob for small files (<64KB); empty for large
}

message FileDelete {}

enum EntryKind {
  FILE = 0;
  DIRECTORY = 1;
  SYMLINK = 2;
}

// --- Delta transfer ---

message DeltaRequest {
  bytes target_digest = 1;
  uint32 block_size = 2;
  repeated BlockSignature signature = 3;
}

message BlockSignature {
  uint32 index = 1;
  uint32 weak_hash = 2;
  bytes strong_hash = 3;
}

message DeltaResponse {
  bytes target_digest = 1;
  uint64 target_size = 2;
  repeated DeltaOp ops = 3;
}

message DeltaOp {
  oneof op {
    uint32 copy_index = 1;
    bytes data = 2;
  }
}
```

### Endpoints

#### `GET /sync/subscribe?since={version}`

Long-lived chunked HTTP response. Length-prefixed protobuf frames:

```
[4 bytes: message length, big-endian][SyncEvent protobuf]
[4 bytes: message length, big-endian][SyncEvent protobuf]
...
```

Small files (<64KB) have content inlined in `FileChange.data`. Large files have `data` empty — client fetches separately.

The server subscribes to the broadcaster first, then backfills from `change_log`, deduplicating by version. This ensures no events are missed during the gap between backfill query and live subscription.

#### `GET /blobs/{digest}`

Serves file content from the watched directory by looking up the digest in the current tree to find the file path. Not CDN-cacheable (file may change between events).

#### `POST /blobs/delta`

Request: `DeltaRequest` protobuf body. Response: `DeltaResponse` protobuf body.

For large files the client has a previous version of. Client sends rsync block signatures of its local copy, hub computes delta against the target file and responds with delta ops.

#### `GET /snapshots-tree/latest`

302 redirect to `/snapshots-tree/{version}`.

#### `GET /snapshots-tree/{version}`

Serves the hub_tree SQLite DB at the given version. Just the tree and version cursor — no file blobs. Clients fetch blobs separately via `/blobs/{digest}`.

#### `POST /push`

See `client-read-write-spec.md`. Used by write-mode clients to push local changes back.

---

## Client

### Architecture

The client is a read-only replica. It receives changes from the hub and applies them to the local filesystem.

Components:
- HTTP client with Bearer token auth
- Protobuf stream decoder (length-prefixed frames)
- rsync delta engine (for large file updates)
- SQLite DB (hub tree + sync state) via sqlx

### Client Storage

Single SQLite database. Same schema as the snapshot DB:

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

CREATE INDEX idx_hub_tree_digest ON hub_tree(digest);

CREATE TABLE sync_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
```

The `hub_tree` table doubles as a digest-to-path index. When the client receives a FileChange for a large file, it can check whether the digest already exists locally before fetching:

```sql
SELECT path FROM hub_tree WHERE digest = ? AND path != ? LIMIT 1;
```

If a match is found, copy the local file instead of fetching from the hub. This handles moves, copies, and duplicate files — no rename event needed in the protocol.

### Sync Flows

#### First Connect (Bootstrap)

```
Client                                    Hub
  |                                         |
  |-- GET /snapshots-tree/latest --------> |
  | <-- 302 /snapshots-tree/500 ---------- |
  |                                         |
  |-- GET /snapshots-tree/500 -----------> |
  | <---- SQLite DB body ----------------- |
  |                                         |
  [import tree DB into client's hub_tree]   |
  [hub_version=500, tree ready]            |
  |                                         |
  [for each unique digest in tree:]         |
  |-- GET /blobs/{digest} ---------------> |
  | <---- blob content ------------------- |
  | ...                                     |
  |                                         |
  |-- GET /sync/subscribe?since=500 -----> |
  | <-- stream: events 501.. ------------- |
```

Two-step bootstrap:
1. Fetch the tree DB — cheap, one round trip, gives the client all metadata
2. Walk the tree, fetch each unique blob via `/blobs/{digest}` — deduplicated by digest, parallelizable over HTTP/2

Then resume incremental sync from the snapshot version.

Metadata-only clients (e.g. Rust client) can skip step 2 entirely and fetch blobs lazily on demand.

#### Incremental Sync

For each SyncEvent received:

```
SyncEvent(version=V, path=P):

  FileChange(kind, digest, size, mode, mtime, data):
    if data present (small file, ≤64KB):
      write data to local FS at P
    else (large file):
      1. dedup: check hub_tree for matching digest at different path
         -> hit: copy local file
      2. delta: if local file exists at P with different content
         -> send block signatures, apply delta, verify hash
      3. full fetch: GET /blobs/{digest}

    INSERT OR REPLACE INTO hub_tree
    UPDATE sync_state hub_version = V
    (in a single transaction)

  FileDelete:
    delete local file at P
    DELETE FROM hub_tree WHERE path = P
    UPDATE sync_state hub_version = V
    (in a single transaction)
```

Each event is one SQLite transaction + one filesystem write. Crash-safe — on restart, re-open stream from last persisted `hub_version`.

#### Reconnect

Client reconnects with 1-second backoff on connection loss. On reconnect, resumes from the last persisted `hub_version`.

### Pull Decision Tree

```
SyncEvent for path P, digest D, size S:

  data field present (S ≤ 64KB)?
    -> write inline data to FS. Done.

  data field empty (S > 64KB):

    1. Dedup: SELECT path FROM hub_tree WHERE digest = D AND path != P LIMIT 1
       hit?  -> copy local file. Done.

    2. Delta: local file exists at P?
       yes?  -> compute block signatures of local file
               POST /blobs/delta with signatures
               apply delta ops, verify SHA-256 == D. Done.

    3. Full fetch: GET /blobs/D
```

### Delta Transfer

rsync algorithm for large file updates:

- Client divides local file into fixed blocks
- Block size heuristic: `sqrt(24 * fileSize)`, clamped to [1KB, 64KB]
- Computes per block: weak rolling checksum (classic rsync two-component) + SHA-256 strong hash
- Sends signature to hub via `POST /blobs/delta`
- Hub scans target file with sliding window, matches blocks via `map[uint32][]BlockSig`
- Returns delta: `copy_index` ops (reuse client block) + `data` ops (new bytes)
- Client reconstructs file, verifies SHA-256 digest matches target

For a 10MB file with a small edit: ~30KB up + ~few KB down vs 10MB full fetch.

Falls back to full fetch if delta transfer fails for any reason.

### Setting Local mtime

When writing files (from snapshot extraction or SyncEvent), the client sets the local file's mtime to match the hub's mtime from the event. This prepares for future read-write mode where local scan can skip unchanged files.

---

## CLI

```bash
# Start hub server
HUBSYNC_TOKEN=secret hubsync serve -dir /some/dir -listen 127.0.0.1:8080

# Start read-only client sync
HUBSYNC_TOKEN=secret hubsync client -hub http://localhost:8080 -dir /replica

# Start read-write client (pushes local changes back)
HUBSYNC_TOKEN=secret hubsync client -hub http://localhost:8080 -dir /replica -mode write
```

Flags:
- `serve`: `-dir` (directory to watch, default `.`), `-listen` (address, default `127.0.0.1:8080`), `-db` (database path, default `<dir>/.hubsync/hub.db`)
- `client`: `-hub` (hub URL, required), `-dir` (sync directory, default `.`), `-db` (database path, default `<dir>/.hubsync/client.db`), `-mode` (`read` or `write`, default `read`), `-scan-interval` (write mode scan interval, default `5s`)

---

## Implementation Details

- **Language:** Go
- **SQLite driver:** `modernc.org/sqlite` (pure Go, no CGo)
- **SQLite access:** `jmoiron/sqlx`
- **Protobuf:** `google.golang.org/protobuf`
- **Compression:** `klauspost/compress/zstd`
- **Filesystem watching:** `fsnotify/fsnotify` with 50ms debounce
- **Ignore filtering:** `hayeah/go-lstree` (Ignorer interface)
- **DI:** `google/wire` (compile-time dependency injection)
- **Subscriber broadcast:** channel-per-subscriber (`map[chan ChangeEntry]struct{}`), buffered (256), non-blocking sends

---

## Read-Write Mode

Implemented — see `client-read-write-spec.md`. Highlights:

- `POST /push` endpoint with optimistic concurrency (`base_version`)
- PushOp, PushRequest, PushResponse protobuf messages
- No content-level merging — files are opaque blobs (like Dropbox/mutagen)
- Conflict policies: `FAIL` (default), `HUB_WINS`, `CLIENT_WINS`
- On conflict, client renames local file to `(conflicted copy)` and fetches the hub's version
- Client-side local tree scanning and diff against `hub_tree`
- Periodic scan + push loop in write mode

## Still Deferred

- Client-side filesystem watcher (currently timer-based scanning only)
- Log compaction / GC
