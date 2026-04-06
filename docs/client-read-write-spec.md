---
overview: Spec for hubsync client read/write permissions. Two permission levels (read, write). Adds push endpoint, auto-merge, conflict resolution, and client-side scanning to the existing read-only implementation.
repo: ~/github.com/hayeah/hubsync
tags:
  - spec
  - hubsync
---

# HubSync: Client Read/Write Spec

Extends the read-only sync system (see `docs/spec.md`) with write support. Two permission levels: **read** and **write**.

## Authentication

All endpoints require a valid bearer token (`HUBSYNC_TOKEN`). A valid token grants full read/write access — no per-client permission levels.

## Protocol Additions

### New Protobuf Messages

Added to the existing protobuf definitions:

```protobuf
// --- Push ---

message PushRequest {
  repeated PushOp ops = 1;
  ConflictPolicy on_conflict = 2;
}

message PushOp {
  string path = 1;
  OpKind op = 2;
  uint64 base_version = 3;  // version this edit is based on (from hub_tree.version)
  bytes digest = 4;          // SHA-256 of new content
  bytes data = 5;            // blob content (inline)
}

enum OpKind {
  CREATE = 0;
  UPDATE = 1;
  DELETE = 2;
}

enum ConflictPolicy {
  FAIL = 0;           // partial success, conflicts returned
  HUB_WINS = 1;       // drop conflicting client changes silently
  CLIENT_WINS = 2;    // force overwrite hub (requires write permission)
}

message PushResponse {
  repeated PushResult results = 1;
}

message PushResult {
  string path = 1;
  oneof outcome {
    PushAccepted accepted = 2;
    PushConflict conflict = 3;
  }
}

message PushAccepted {
  uint64 version = 1;       // new version assigned by hub
  bool merged = 2;           // true = hub auto-merged, client should pull result
  string merge_strategy = 3; // e.g. "text", "json", "append"
}

message PushConflict {
  string reason = 1;
  uint64 current_version = 2;
  bytes current_digest = 3;
}
```

### New Endpoint

#### `POST /push`

Request: `PushRequest` protobuf. Response: `PushResponse` protobuf.

Blob content is inline in `PushOp.data`.

Each `PushOp` carries a `base_version` — the version of the file the client last saw (from its `hub_tree.version` column). The hub uses this for optimistic concurrency:

- **base_version matches current** — accept directly
- **base_version is stale** — attempt auto-merge
- **auto-merge fails** — return conflict

The `on_conflict` field controls fallback:

- **`FAIL`** (default) — partial success, conflicts returned for resolution
- **`HUB_WINS`** — silently drop conflicting ops (reported in response). Good for fire-and-forget agents
- **`CLIENT_WINS`** — force overwrite. For trusted editors

## Hub-Side Auto-Merge

When a push arrives with a stale base version, the hub has all three versions:

```
base   = blob at the client's base_version (from change_log / blob store)
theirs = blob at current hub version
ours   = blob from client push
```

The hub runs a text three-way merge (diff3) on the three versions. If no overlapping hunks, produce the merged result. Same algorithm as `git merge`. If there are overlapping hunks, return a conflict.

### Blob Retention for Merge

Auto-merge requires keeping historical blobs. Since the hub serves blobs from the watched directory (no blob table), it needs to retain old versions for merge:

- Keep a `merge_blobs` table for recently-overwritten content
- Retention window: configurable (e.g. last 1000 versions or 24 hours)
- Client whose `base_version` is older than the retention window gets a conflict with reason `"base_too_old"` — client must pull and retry

```sql
CREATE TABLE merge_blobs (
  version   INTEGER PRIMARY KEY,
  path      TEXT NOT NULL,
  digest    TEXT NOT NULL,
  data      BLOB NOT NULL
);
```

## Write Client Design

### Client-Side Local Tree Scanning

A write client needs to detect local changes. It compares local filesystem state against the `hub_tree` to produce a diff.

```
scan(sync_dir, hub_tree):
  for each file in walk(sync_dir):
    stat(file) -> (mtime, size)
    hub_entry = hub_tree.get(relative_path)

    if hub_entry and (mtime, size) match:
      -> in sync, skip (digest cache hit via hub_tree)

    local_digest = hash(file)
    if hub_entry and local_digest == hub_entry.digest:
      -> in sync despite stat change, skip

    yield LocalChange(path, local_digest, hub_entry)

  for hub_entry not found on local FS:
    yield LocalDeletion(path, hub_entry)
```

The hub tree acts as both the "expected state" and the digest cache — no separate cache needed.

### Diff to PushOps

```
diff(scan_results):
  for each LocalChange(path, digest, hub_entry):
    if hub_entry is None:
      -> PushOp(path, CREATE, base_version=0, digest, data)
    else:
      -> PushOp(path, UPDATE, base_version=hub_entry.version, digest, data)

  for each LocalDeletion(path, hub_entry):
    -> PushOp(path, DELETE, base_version=hub_entry.version)
```

### Push Flow

```
Client                              Hub
  |                                   |
  [scan local FS]                     |
  [diff against hub_tree]             |
  |                                   |
  |-- POST /push ------------------> |
  |   ops: [b.txt=UPDATE,            |
  |          c.txt=DELETE,            |
  |          d.txt=CREATE]            |
  |   on_conflict: FAIL               |
  |                                   |
  |             [b.txt: stale → try merge]
  |             [  text 3-way → success]
  |             [c.txt: version matches → accept]
  |             [d.txt: new → accept]
  |                                   |
  | <---- PushResponse ------------- |
  |   results: [b=accepted(merged),  |
  |             c=accepted,           |
  |             d=accepted]           |
  |                                   |
  [update hub_tree for c, d]         |
  [b was merged: pull merged blob    |
   via sync stream]                   |
```

After push:

- **Accepted (not merged)**: update `hub_tree` entry with new version and digest
- **Accepted (merged)**: the hub's merged result differs from what we sent. The client will receive the merged version via the sync stream — update hub_tree and local file then
- **Conflict**: surface to caller for resolution

### Reconnect with Local Edits

```
Client comes online:
  - Load hub_tree, read hub_version
  - Open sync stream since=hub_version
  - Receive events, update hub_tree in memory
    (DO NOT overwrite local files for paths with local edits)
  - Scan local FS, diff against updated hub_tree
  - POST /push with diff
  - Handle conflicts
```

Local edits survive reconnect — they're on the filesystem. The hub tree updates independently. The diff catches everything.

### Conflict Resolution Strategies

When auto-merge fails and `on_conflict=FAIL`, the client gets a conflict list. Resolution options:

| Strategy | Behavior | Good for |
|---|---|---|
| **hub-wins** | Discard local, accept hub version | Agents that can regenerate |
| **client-wins** | Re-push with `on_conflict=CLIENT_WINS` | Trusted editors |
| **fork** | Save local as `.conflict` file, pull hub version | Human review later |
| **retry** | Pull latest, re-do local work, push again | Agents with deterministic output |

The resolution strategy is chosen by the caller (app or agent), not configured in the client library.

## Go Client Additions

The existing Go CLI client (`hubsync client`) gains write support:

- `-mode read|write` flag (default: `read`)
- In write mode: periodic local scan + push loop
- Scan interval configurable (default: 5s)
- Uses `.gitignore` filtering (same as hub) when scanning

### Write Mode Loop

```
loop:
  - Wait for scan trigger (timer or FS watcher event)
  - Scan local FS, diff against hub_tree
  - If changes found: POST /push
  - Handle response:
    - accepted: update hub_tree
    - merged: wait for sync stream to deliver merged version
    - conflict: log warning, skip (hub-wins by default for CLI)
```

### Client-Side FS Watcher (Optional)

For responsive push in write mode, the client can use `fsnotify` to detect local changes immediately rather than waiting for the scan timer. Same debounce logic as the hub (50ms). Falls back to timer-based scanning if watcher errors.

## Rust Client Additions

The Rust client (see `docs/rust-client-spec.md`) is currently read-only + SQLite-only (no FS writes). Write support for the Rust client is a separate concern — it would require:

- Local filesystem materialization (writing synced files to disk)
- Local scanning + diffing
- Push API

This is deferred. The Rust client remains read-only for now.

## What This Spec Does NOT Change

- Hub storage schema (change_log, no blob table) — unchanged
- Sync stream protocol — unchanged
- Snapshot format — unchanged
- Delta transfer — unchanged
- Read client behavior — unchanged
- Rust client — remains read-only
