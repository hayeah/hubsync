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

# Scaffold .hubsync/config.toml (optional; only needed for B2 archive)
./hubsync init /path/to/files

# Start hub (watches current directory)
HUBSYNC_TOKEN=secret ./hubsync serve -dir /path/to/files -listen 127.0.0.1:8080

# Start read-only client
HUBSYNC_TOKEN=secret ./hubsync client -hub http://localhost:8080 -dir /path/to/replica

# Start read-write client (pushes local changes back)
HUBSYNC_TOKEN=secret ./hubsync client -hub http://localhost:8080 -dir /path/to/replica -mode write

# One-shot sync (bootstrap and/or catch up, then exit)
HUBSYNC_TOKEN=secret ./hubsync client -hub http://localhost:8080 -dir /path/to/replica -once
```

## Backblaze B2 archive

Optionally, a hub can back itself up to Backblaze B2 and let the operator evict local copies via `hubsync unpin`. Bootstrap a default config with:

```bash
hubsync init /path/to/hub
```

That drops `.hubsync/config.toml` (see `docs/config.example.toml`) with an `[archive]` section that picks its values up from the process env:

```toml
[hub]
hash = "xxh128"                         # or "sha256"; set once at init

[archive]
provider      = "b2"
bucket        = "${HUBSYNC_BUCKET}"
bucket_prefix = "${HUBSYNC_BUCKET_PREFIX}"
b2_key_id     = "${B2_APPLICATION_KEY_ID}"
b2_app_key    = "${B2_APPLICATION_KEY}"
```

`LoadConfigFile` runs `os.ExpandEnv` over the `[archive]` string fields (`bucket`, `bucket_prefix`, `b2_key_id`, `b2_app_key`) so you can:

- export the vars in your shell,
- prefix commands with `godotenv -f ~/.env.secret hubsync …`,
- use direnv / mise env / whatever,

…and keep secrets out of the committed config. If `bucket_prefix` resolves empty (unset env var or literal empty), it defaults to `<hub-dir-basename>/` — so one exported `HUBSYNC_BUCKET` serves N hubs, each with its own prefix.

If you'd rather not use env vars, just edit `.hubsync/config.toml` and hard-code the values. `hubsync init` refuses to overwrite an existing file.

When the config is present, `hubsync serve` starts an archive worker that uploads every file under the hub directory to `<bucket>/<bucket_prefix>`. Each upload stamps `X-Bz-Info-hubsync_digest` + `X-Bz-Info-hubsync_digest_algo` so an operator (or a future `fsck`) can cross-check content.

Operator verbs work with **or without** a running `hubsync serve`:

```bash
hubsync archive /path/to/dir              # one-shot: scan + upload pending files, then exit
hubsync archive /path/to/dir --dry        # preview as JSONL; no network, no state change
hubsync ls                                # virtual listing incl. unpinned entries
hubsync status                            # tree + archive counts
hubsync pin   'big.bin'                   # ensure the file is archived + local
hubsync unpin 'big.bin'                   # ensure it's archived, then evict local
hubsync unpin '**/*.tmp'  --dry           # show the plan, mutate nothing
```

If `hubsync serve` is listening on `.hubsync/serve.sock`, these verbs RPC
to it. If it isn't, they run in-process: take an exclusive flock on
`.hubsync/hub.lock` (so two writers can't race), scan the tree, apply the
operation, release the lock. `ls` and `status` are read-only and skip the
lock entirely — they open the DB read-only via SQLite WAL. This makes
`hubsync archive` usable as a general "back up this tree to B2 and leave"
tool with no daemon required.

**Pin state is a data-fetch concern, not a tree-visibility concern.** Clients still see unpinned rows in `hub_tree`; `GET /blobs/{digest}` on a hub whose local copy is gone transparently returns a 302 to a short-lived presigned B2 URL.

## CLI

### `hubsync init`

Scaffold `.hubsync/config.toml` under `dir` (default: current directory) from the embedded template. Refuses to overwrite an existing config — remove it by hand to re-init. Never touches `hub.db`, `serve.sock`, or other files under `.hubsync/`.

```
hubsync init [dir]
```

The scaffolded config references env vars via `${VAR}` interpolation for bucket / prefix / B2 credentials. See "Backblaze B2 archive" above.

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
| `-once` | `false` | Bootstrap and/or catch up to the hub's current state, then exit (read mode only) |

### `hubsync archive`

One-shot: walks a hub directory and uploads every pending file to B2, then exits. Complementary to `hubsync serve` — use when you want to back up a static tree (e.g. a build output) without running a daemon.

```
hubsync archive [dir] [--dry] [--quiet]
```

| Flag | Default | Description |
|---|---|---|
| positional `dir` | `.` | Hub directory (walks up looking for `.hubsync/`) |
| `--dry` | `false` | Emit JSONL of what would upload; no network, no state change, no B2 credentials required |
| `--quiet` | `false` | Suppress per-file progress on stderr |

Takes `.hubsync/hub.lock` for the duration, so concurrent runs against the same hub fail fast with a clear error. Exit codes: `0` = all uploaded (or nothing to do, or `--dry` succeeded); `1` = at least one upload failed (others still attempted); `2` = startup failure (no `.hubsync`, missing `[archive]`, lock held, etc.).

```bash
# Typical usage in a build pipeline
hubsync init ./dist
godotenv -f ~/.env.secret hubsync archive ./dist

# Preview before running the real thing
hubsync archive ./dist --dry | duckql "SELECT state, count(*) GROUP BY 1"
```

### `hubsync archive-gc`

One-shot: lists a B2 prefix and deletes objects that no local `hub_entry` row claims. Useful for retiring orphaned test cruft, saturation-probe bytes, or whole sid/ trees that nobody owns anymore.

```
hubsync archive-gc <prefix> [--dry] [-dir DIR]
```

| Flag | Default | Description |
|---|---|---|
| positional `prefix` | (required) | Bucket-relative key prefix. Literal string, not a glob. |
| `--dry` | `false` | Classify without deleting; emit JSONL of would-be actions. |
| `-dir` | `.` | Hub directory (walks up looking for `.hubsync/`). Hard error if not found. |

`archive-gc` is a thin wrapper around B2's native list primitive: **one invocation ↔ one delimited list call at the given prefix** (plus a flat sweep per orphan dir we actually delete). Each listed entry is either a real file or a common-prefix "dir" marker, and is classified against `hub_entry`:

- **File** — kept if some `hub_entry` row's `bucket_prefix + path` equals the key; otherwise deleted.
- **Dir** (common prefix, ends in `/`) — kept if any `hub_entry` row lives under that prefix; otherwise the whole subtree is deleted.

There's no `--depth N` flag because B2's list API is single-level — if you want to audit *inside* a kept dir, re-run with a deeper prefix. Keeping invocations 1:1 with the API keeps the cost and blast radius obvious at each step.

`archive-gc` does **not** take `.hubsync/hub.lock`. It reads `hub.db` through SQLite WAL (safe alongside a running `serve`) and only writes to B2. A per-object fileID guard on `DeleteByKey` protects against the list→delete race where a new version could have been uploaded at the same key — that guard is free (piggybacks on blazer's implicit HEAD inside `Object.Delete`).

Exit codes: `0` = success (or `--dry` succeeded); `1` = one or more deletes failed (others still attempted); `2` = startup failure (no `.hubsync`, missing `[archive]`, bad prefix).

Flags must precede the positional (Go's `flag` package stops at the first non-flag).

**Does NOT retire tracked rows.** If a path has a `hub_entry` row, `archive-gc` keeps it — even if you'd prefer it gone. Retiring a tracked row (evicting the DB row + the B2 copy + the local file together) is a separate concern with its own eventual verb.

**Cost note**: B2's Class C list transactions are generously free-tiered (2,500/day). Per-object deletes are Class A (not free) — for large time-based cleanup of whole prefixes (e.g. "everything under `b2cast-test/` older than 14 days"), a B2 bucket-level lifecycle rule is free and strictly cheaper than `archive-gc`. Use `archive-gc` for targeted, claim-aware cleanup; use lifecycle rules for blanket retention policy.

```bash
# Preview: what would get deleted under this prefix?
hubsync archive-gc --dry b2cast-test/2026-04-17/ | duckql \
  "SELECT action, count(*) FROM stdin GROUP BY 1"

# Actually clean up a bucket-side-only orphan prefix the hub never tracked.
# (Run from inside any hub whose bucket is the same one — the hub's rows all
# live under its own bucket_prefix, so nothing matches these keys: everything
# is classified as orphan.)
hubsync archive-gc b2cast-saturate/2026-04-17/
```

### `hubsync pin` / `hubsync unpin`

Declarative commands; the argument is a doublestar glob, the command is the target state.

| Flag | Default | Description |
|---|---|---|
| `-dir` | `.` | Hub directory (walks up looking for `.hubsync/`) |
| `--dry` | `false` | Print the reconciler plan per path; do not mutate |

If `hubsync serve` is running, the verbs RPC to it. Otherwise they run in-process after taking `.hubsync/hub.lock`. Either way `[archive]` must be configured (pin/unpin need the B2 client). `--dry` must precede the glob (Go's `flag` package stops at the first positional).

### `hubsync ls`

Virtual listing from the hub's registry (including unpinned rows). Follows the shared [`ls` / `duckql` convention](https://github.com/hayeah/dotfiles) — no bespoke filter flags; pipe to `duckql` when you want to filter, sort, or aggregate.

```
hubsync ls [--quiet]
```

| Flag | Default | Description |
|---|---|---|
| `-dir` | `.` | Hub directory |
| `--quiet` | `false` | Suppress warnings on stderr (no-op today — convention placeholder) |

Output is **always JSONL**, one row per `hub_entry` row. For a human table on the terminal, pipe to `duckql` (which handles TTY formatting).

The wire shape is literally the `HubEntry` Go struct serialized through its JSON tags — no projection type, no synthesis. If a field exists on the struct, it shows up in the output, named after the SQL column it came from:

```go
type HubEntry struct {
    Path              string       `json:"path"`
    Kind              FileKind     `json:"kind"`                          // "file" | "directory" | "symlink"
    Digest            Digest       `json:"digest,omitempty"`              // hex; omitted when NULL
    Size              int64        `json:"size"`
    Mode              uint32       `json:"mode"`                          // POSIX mode bits, decimal
    MTime             int64        `json:"mtime"`                         // unix seconds
    Version           int64        `json:"version"`                       // change_log version
    ArchiveState      ArchiveState `json:"archive_state"`                 // "" | "dirty" | "archived" | "unpinned"
    ArchiveFileID     string       `json:"archive_file_id,omitempty"`
    ArchiveSHA1       Digest       `json:"archive_sha1,omitempty"`        // hex; omitted until uploaded
    ArchiveUploadedAt int64        `json:"archive_uploaded_at,omitempty"` // unix millis
    UpdatedAt         int64        `json:"updated_at"`                    // unix seconds
}
```

`Digest` (used for both `digest` and `archive_sha1`) is a `type Digest string` whose `MarshalJSON` emits lowercase hex — matching `sqlite3`'s `hex(digest)` encoding. `FileKind` is an `int` enum whose `MarshalJSON` emits one of the three labels above.

A sample row:

```json
{
  "path": "b9a2f659…/thumbs/thumbs_016.webp",
  "kind": "file",
  "digest": "5c3bde711c5da356b4f2502b7d9049fd",
  "size": 156064,
  "mode": 420,
  "mtime": 1776428310,
  "version": 4929,
  "archive_state": "archived",
  "archive_file_id": "4_z72d87f2f15d395a19dd20010_f115965cef5a50e90_…",
  "archive_sha1": "5b9c5ba240a70ae931c52dd98e3600f5d636b6a3",
  "archive_uploaded_at": 1776428563163,
  "updated_at": 1776428563
}
```

`hubsync archive --dry` emits the **same** row shape, so any query that works for `ls` works for the dry-run preview.

#### Examples with `duckql`

The output is a plain stream of JSON rows; everything below assumes `duckql` is on `$PATH` (see [the duckql skill](https://github.com/hayeah/duckql)). DuckDB function names apply — `regexp_extract`, `to_timestamp`, `strftime`, `sum`, `percentile_cont`, etc.

**Filter & count**

```bash
# Every unpinned row (remote-only; local bytes evicted)
hubsync ls | duckql "WHERE archive_state='unpinned'"

# Health check: how many rows in each archive state, with total bytes?
hubsync ls | duckql "
  SELECT archive_state,
         count(*) AS n,
         round(sum(size)/1e9, 2) AS gb
  GROUP BY 1 ORDER BY n DESC"

# Rows still pending the archive worker (NULL or dirty)
hubsync ls | duckql "
  SELECT count(*) AS pending,
         round(sum(size)/1e6, 1) AS mb
  WHERE archive_state IN ('', 'dirty') AND kind='file'"
```

**Size & layout**

```bash
# Ten biggest files, in MB
hubsync ls | duckql "
  SELECT path, round(size/1e6, 1) AS mb
  WHERE kind='file'
  ORDER BY size DESC LIMIT 10"

# Bytes by first path segment (one source_id per line in a b2cast-style tree)
hubsync ls | duckql "
  SELECT regexp_extract(path, '^([^/]+)', 1) AS prefix,
         count(*) AS n,
         round(sum(size)/1e9, 2) AS gb
  WHERE kind='file'
  GROUP BY 1 ORDER BY gb DESC"

# Bytes by file extension
hubsync ls | duckql "
  SELECT regexp_extract(path, '\.([^./]+)\$', 1) AS ext,
         count(*) AS n,
         round(sum(size)/1e9, 2) AS gb
  WHERE kind='file'
  GROUP BY 1 ORDER BY gb DESC"
```

**Time-based**

```bash
# Upload throughput per minute (reads archive_uploaded_at, which is unix millis)
hubsync ls | duckql "
  SELECT strftime(to_timestamp(archive_uploaded_at/1000), '%Y-%m-%d %H:%M') AS minute,
         count(*) AS uploads,
         round(sum(size)/1e6, 1) AS mb
  WHERE archive_state='archived'
  GROUP BY 1 ORDER BY minute DESC LIMIT 10"

# Ten most recently archived rows
hubsync ls | duckql "
  SELECT path,
         to_timestamp(archive_uploaded_at/1000) AS at
  WHERE archive_state='archived'
  ORDER BY archive_uploaded_at DESC LIMIT 10"
```

**Content-addressed**

```bash
# Duplicate content: same digest at multiple paths
hubsync ls | duckql "
  SELECT digest, count(*) AS n, any_value(path) AS sample_path
  WHERE kind='file'
  GROUP BY 1
  HAVING count(*) > 1
  ORDER BY n DESC LIMIT 20"

# Find a specific B2 object by its archive_file_id
hubsync ls | duckql "WHERE archive_file_id LIKE '%f115965cef5a50e90%'"
```

**Archive preview**

```bash
# `archive --dry` emits the same row shape, so any of the above queries work
# against it too — useful for previewing a run before touching B2.
hubsync archive --dry | duckql "
  SELECT count(*) AS pending,
         round(sum(size)/1e9, 2) AS gb"
```

### `hubsync status`

Grouped archive counts.

| Flag | Default | Description |
|---|---|---|
| `-dir` | `.` | Hub directory |
| `--json` | `false` | Machine-readable output |

### Environment Variables

| Variable | Description |
|---|---|
| `HUBSYNC_TOKEN` | Bearer token for auth. Set on both hub and client. If unset on hub, no auth required. |
| `B2_APPLICATION_KEY_ID` | B2 app key ID. Used via `${B2_APPLICATION_KEY_ID}` in the scaffolded `[archive] b2_key_id`, or as a direct fallback when that field is empty. |
| `B2_APPLICATION_KEY` | B2 app key secret (same pattern as above). |
| `HUBSYNC_BUCKET` | B2 bucket name, via `${HUBSYNC_BUCKET}` in the scaffolded `[archive] bucket`. |
| `HUBSYNC_BUCKET_PREFIX` | B2 object-key prefix, via `${HUBSYNC_BUCKET_PREFIX}` in the scaffolded `[archive] bucket_prefix`. When empty, defaults to `<hub-dir-basename>/`. |

## Architecture

```
Hub Server                              Client(s)
+- HubStore (change_log + hub_entry)    +- ClientStore (hub_tree)
+- Scanner (lstree, .gitignore)         +- Sync stream consumer
+- Watcher (fsnotify, 50ms)             +- Push loop (write mode)
+- Broadcaster (channels)               +- Delta engine (rsync)
+- HTTP Server                          +- Conflicted copy handler
+- ArchiveWorker  (optional, B2)
+- RPCServer (unix socket, pin/unpin)
```

## Protocol

All endpoints require `Authorization: Bearer <token>` when `HUBSYNC_TOKEN` is set. Streaming uses length-prefixed protobuf (4-byte big-endian size + protobuf bytes).

### Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/sync/subscribe?since={version}` | Stream changes as `SyncEvent` messages |
| GET | `/blobs/{digest}` | Fetch file content by raw-hash hex digest (xxh128 or sha256). For unpinned paths on archive-configured hubs, returns `302` to a presigned B2 URL. |
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

### One-shot mode

`hubsync client -once` performs a single reconciliation pass and exits — useful for cron, CI, or scripts:

1. Fetches the latest tree DB
2. Skips local files whose digest already matches the hub
3. Fetches missing/changed blobs
4. Removes local files no longer in the tree
5. Exits with a summary like `catchup complete, version=42, fetched=3, skipped=17, deleted=1`

Idempotent — safe to re-run. Read mode only.

### Bootstrap

New clients bootstrap by fetching the tree DB and then each blob individually:

- `GET /snapshots-tree/latest` — download the hub_tree SQLite DB (file metadata + version cursor)
- Walk the tree, fetch each unique blob via `GET /blobs/{digest}` (deduplicated by digest)
- Connect to sync stream from the snapshot version for incremental updates

This is HTTP/2 friendly (parallel blob fetches over one connection) and CDN-cacheable (`/blobs/{digest}` is content-addressed and infinitely cacheable).

For metadata-only clients (e.g. the Rust client which is SQLite-only, no FS writes), just the tree DB fetch is sufficient — skip blob fetching entirely.

## Database Schema

All digests are raw-hash `BLOB` values. Length depends on `[hub] hash`: 16 bytes for xxh128 (default on new hubs), 32 bytes for sha256. Read with `SELECT hex(digest) …` in the sqlite3 shell.

### Hub: `change_log` (append-only)

```sql
CREATE TABLE change_log (
  version   INTEGER PRIMARY KEY AUTOINCREMENT,
  path      TEXT NOT NULL,
  op        TEXT NOT NULL CHECK(op IN ('create', 'update', 'delete')),
  kind      INTEGER NOT NULL DEFAULT 0,
  digest    BLOB,
  size      INTEGER,
  mode      INTEGER,
  mtime     INTEGER NOT NULL
);
```

### Hub: `hub_entry` (materialized tree + archive state)

```sql
CREATE TABLE hub_entry (
  path                TEXT PRIMARY KEY,
  kind                INTEGER NOT NULL,
  digest              BLOB,
  size                INTEGER,
  mode                INTEGER,
  mtime               INTEGER,
  version             INTEGER NOT NULL,
  archive_state       TEXT,           -- NULL | 'dirty' | 'archived' | 'unpinned'
  archive_file_id     TEXT,
  archive_sha1        BLOB,
  archive_uploaded_at INTEGER,
  updated_at          INTEGER NOT NULL
);
```

Scanner/watcher own `path, kind, digest, size, mode, mtime, version`. Archive worker owns the `archive_*` columns. No column is shared.

### Client: `hub_tree` (materialized state)

```sql
CREATE TABLE hub_tree (
  path    TEXT PRIMARY KEY,
  kind    INTEGER NOT NULL DEFAULT 0,
  digest  BLOB,
  size    INTEGER,
  mode    INTEGER,
  mtime   INTEGER,
  version INTEGER NOT NULL
);
```

The snapshot shipped to clients via `/snapshots-tree/{version}` carries a `sync_state.hash_algo` row so clients can pick the right streaming hash for local content checks.

The `version` column in `hub_tree` is the change_log version that last touched this entry — used as `base_version` when pushing.

## Other Clients

### Rust client (`rust-client/`)

A metadata-only client (no FS materialization) intended for embedding via FFI in mobile/desktop apps. It maintains the `hub_tree` SQLite DB and fetches blobs lazily on demand via `read(path)`. Bootstraps via `/snapshots-tree/latest` (no blob prefetching).

Exposes a C FFI (`hubsync.h`) with a separate bootstrap callback for progress reporting:

```c
typedef void (*HubSyncBootstrapCallback)(uint64_t count, uint64_t total, void *ctx);
```

- `(0, N)` — tree import complete with N entries
- `(count, total)` — blob fetch progress (currently unused — Rust client is metadata-only)

### iOS app (`ios/`)

SwiftUI app that wraps the Rust client via a Swift FFI bridge. Includes a FileProvider extension so the synced files appear in the iOS Files app. Build pipeline at `ios/Makefile.py`.

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
cli/hubsync/main.go     # CLI entry point (serve, client, pin, unpin, ls, status)
archive/archive.go      # ArchiveStorage interface + hubsync fileInfo keys
archive/b2store.go      # blazer-backed ArchiveStorage
archive/fake.go         # in-memory ArchiveStorage for tests
archive_worker.go       # Worker that drains NULL/dirty rows to B2
reconciler.go           # Pin/unpin declarative reconciler
rpc.go                  # Unix-socket RPC server + client
config_file.go          # .hubsync/config.toml loader
hasher.go               # Hasher interface (xxh128, sha256)
docs/spec.md            # Full read-only protocol spec
docs/client-read-write-spec.md  # Write mode spec
AGENTS.md               # Testing process for AI agents
```

## Testing

See **AGENTS.md** for the three testing tiers (unit, live-bucket integration, manual smoke). Quick reference:

```bash
# Tier 1 — unit + e2e, no network
go test ./...

# Tier 2 — live B2 (gated on env)
HUBSYNC_TEST_BUCKET=hayeah-hubsync-test godotenv -f ~/.env.secret go test -v ./archive/

# Tier 3 — full serve + CLI smoke: see AGENTS.md
```

E2E tests spin up an in-process hub + httptest server + client. Archive worker / reconciler / RPC tests use `archive.FakeStorage`. Live-bucket tests skip cleanly when the three env vars (`B2_APPLICATION_KEY_ID`, `B2_APPLICATION_KEY`, `HUBSYNC_TEST_BUCKET`) aren't all set.
