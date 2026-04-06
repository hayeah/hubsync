---
overview: Spec for hubsync client read/write permissions. Adds push endpoint, optimistic concurrency, conflicted copy resolution, and client-side scanning to the existing read-only implementation.
repo: ~/github.com/hayeah/hubsync
tags:
  - spec
  - hubsync
---

# HubSync: Client Read/Write Spec

Extends the read-only sync system (see `docs/spec.md`) with write support.

## Design Principles

- **Files are opaque blobs** — no content-level merging (no 3-way text merge, no diff-patch). Same approach as Dropbox and mutagen.
- **Optimistic concurrency** — pushes carry a `base_version`; the hub rejects stale writes.
- **Conflicted copy** — on conflict, the client's local version is renamed to `(conflicted copy)` and the hub's version is fetched. Agents or users reconcile manually per file.

## Authentication

All endpoints require a valid bearer token (`HUBSYNC_TOKEN`). A valid token grants full read/write access.

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
  OP_CREATE = 0;
  OP_UPDATE = 1;
  OP_DELETE = 2;
}

enum ConflictPolicy {
  FAIL = 0;           // partial success, conflicts returned
  HUB_WINS = 1;       // drop conflicting client changes silently
  CLIENT_WINS = 2;    // force overwrite hub
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

- **base_version matches current** — accept, write to disk, append to change_log, broadcast
- **base_version is stale** — conflict (no auto-merge)

The `on_conflict` field controls fallback:

- **`FAIL`** (default) — return conflict with `current_version` and `current_digest` so the client can pull and retry
- **`HUB_WINS`** — silently drop conflicting ops. Good for fire-and-forget agents
- **`CLIENT_WINS`** — force overwrite regardless of version. For trusted editors

No blob retention or merge tables needed — the hub only needs the current file on disk and the current version in the change log.

## Conflict Resolution: Conflicted Copy

When a push returns a conflict (under `FAIL` policy), the client:

- Renames the local file to `<name> (conflicted copy).<ext>`
- Fetches the hub's current blob via `GET /blobs/{digest}` and writes it to the original path
- Logs the conflict for the caller

If a conflicted copy already exists, appends a number: `<name> (conflicted copy 2).<ext>`.

This is easy for agents to handle per-file — they can read both versions, decide which to keep, and push again. No special API needed.

### Resolution Strategies (caller's choice)

| Strategy | Behavior | Good for |
|---|---|---|
| **hub-wins** | Push with `on_conflict=HUB_WINS`, local change dropped | Agents that can regenerate |
| **client-wins** | Re-push with `on_conflict=CLIENT_WINS` | Trusted editors |
| **fork** | Keep the conflicted copy, let user decide | Human review later |
| **retry** | Read hub version, redo work, push again | Agents with deterministic output |

The resolution strategy is chosen by the caller (app or agent), not configured in the client library.

## Write Client Design

### Client-Side Local Tree Scanning

A write client detects local changes by comparing local filesystem state against the `hub_tree`.

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
  |             [b.txt: stale → conflict]
  |             [c.txt: version matches → accept]
  |             [d.txt: new → accept]
  |                                   |
  | <---- PushResponse ------------- |
  |   results: [b=conflict,          |
  |             c=accepted,           |
  |             d=accepted]           |
  |                                   |
  [update hub_tree for c, d]         |
  [b conflicted: rename local to     |
   "b (conflicted copy).txt",        |
   fetch hub's version]              |
```

After push:

- **Accepted**: update `hub_tree` entry with new version and digest
- **Conflict**: rename local to conflicted copy, fetch hub's current blob

### Reconnect with Local Edits

```
Client comes online:
  - Load hub_tree, read hub_version
  - Open sync stream since=hub_version
  - Receive events, update hub_tree in memory
    (DO NOT overwrite local files for paths with local edits)
  - Scan local FS, diff against updated hub_tree
  - POST /push with diff
  - Handle conflicts (conflicted copy)
```

Local edits survive reconnect — they're on the filesystem. The hub tree updates independently. The diff catches everything.

## Go Client Additions

The existing Go CLI client (`hubsync client`) gains write support:

- `-mode read|write` flag (default: `read`)
- `-scan-interval` duration (default: `5s`)
- In write mode: periodic local scan + push loop alongside sync stream
- Uses `.gitignore` filtering (same as hub) when scanning

### Write Mode Loop

```
loop:
  - Wait for scan trigger (timer)
  - Scan local FS, diff against hub_tree
  - If changes found: POST /push
  - Handle response:
    - accepted: update hub_tree
    - conflict: create conflicted copy, fetch hub version
```

### Client-Side FS Watcher (Optional, not yet implemented)

For responsive push in write mode, the client could use `fsnotify` to detect local changes immediately rather than waiting for the scan timer. Same debounce logic as the hub (50ms). Falls back to timer-based scanning if watcher errors.

## Rust Client Additions

The Rust client (see `docs/rust-client-spec.md`) is currently read-only + SQLite-only (no FS writes). Write support for the Rust client is deferred.

## What This Spec Does NOT Change

- Hub storage schema (change_log, no blob table) — unchanged
- Sync stream protocol — unchanged
- Snapshot format — unchanged
- Delta transfer — unchanged
- Read client behavior — unchanged
- Rust client — remains read-only
