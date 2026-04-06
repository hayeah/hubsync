---
overview: Spec for a Rust hubsync client designed for iOS embedding. Stores tree metadata + content in SQLite (no filesystem writes). On-demand content fetching with LRU eviction. Apps query SQLite directly as a virtual filesystem.
repo: ~/github.com/hayeah/hubsync
tags:
  - spec
---

# HubSync Rust Client: Embeddable Read-Only Sync

A Rust client library for hubsync, designed to be embedded in iOS (and other) apps via FFI. Instead of writing files to disk, it stores everything in SQLite — the app queries the database directly as a virtual filesystem.

## Design Goals

- **Embeddable**: compile to a static library, expose C-compatible FFI for Swift/Kotlin
- **SQLite-native**: tree metadata and cached content live in one SQLite database — no filesystem writes
- **Lazy fetch**: sync the tree (metadata) eagerly, fetch content on demand
- **Evictable**: cached content can be evicted under storage pressure, re-fetched later
- **Queryable**: the app reads `hub_tree` directly with SQL — browse, search, filter without going through the client API

## Storage Schema

One SQLite database. The app can open it read-only alongside the client.

```sql
-- Always-synced tree metadata. The "directory listing."
-- Apps query this directly to browse the file tree.
CREATE TABLE hub_tree (
  path      TEXT PRIMARY KEY,
  kind      INTEGER NOT NULL DEFAULT 0,  -- 0=file, 1=dir, 2=symlink
  digest    TEXT,                         -- hex SHA-256, NULL for dirs
  size      INTEGER,
  mode      INTEGER,
  mtime     INTEGER,                     -- unix seconds
  version   INTEGER NOT NULL
);

CREATE INDEX idx_hub_tree_digest ON hub_tree(digest);

-- Content-addressed blob cache. Populated on demand, evictable.
CREATE TABLE content_cache (
  digest      TEXT PRIMARY KEY,
  data        BLOB NOT NULL,
  size        INTEGER NOT NULL,
  fetched_at  INTEGER NOT NULL,   -- unix seconds, when first fetched
  accessed_at INTEGER NOT NULL    -- unix seconds, updated on read (for LRU)
);

-- Sync state.
CREATE TABLE sync_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- rows: ('hub_version', '42'), ('cache_budget', '104857600')
```

### Why One Database

- Atomic: tree updates and content cache share a transaction boundary
- Portable: one file to back up, migrate, or inspect
- No filesystem coordination: no temp files, no partial writes, no cleanup

### Content Status

A file's content status is derived, not stored:

```sql
-- "Is this file's content available locally?"
SELECT
  t.path,
  t.size,
  CASE WHEN c.digest IS NOT NULL THEN 'cached' ELSE 'not_cached' END AS status
FROM hub_tree t
LEFT JOIN content_cache c ON t.digest = c.digest
WHERE t.path LIKE 'docs/%';
```

No explicit state machine. If the row exists in `content_cache`, the content is available. If not, it needs fetching.

## Sync Modes

### Tree Sync (Eager)

Tree metadata is always kept in sync. The client connects to `GET /sync/subscribe?since=N` and applies every event to `hub_tree`:

- `FileChange` → `INSERT OR REPLACE INTO hub_tree`
- `FileDelete` → `DELETE FROM hub_tree`
- Update `sync_state.hub_version` after each event

This is cheap — just metadata, no content. The full tree of a large project is typically <1MB in SQLite.

Content in `content_cache` is NOT evicted when the tree changes. If a file is updated (new digest), the old content remains in cache until LRU eviction. The new content is fetched on next access.

### Content Fetch (On Demand)

When the app needs file content:

```
read(path) -> Result<Vec<u8>>:
  SELECT digest FROM hub_tree WHERE path = ?
  if no row -> NotFound

  SELECT data FROM content_cache WHERE digest = ?
  if hit:
    UPDATE content_cache SET accessed_at = now() WHERE digest = ?
    return data

  fetch from hub:
    1. GET /blobs/{digest}
    2. INSERT INTO content_cache (digest, data, size, fetched_at, accessed_at)
    3. return data
```

Content is cached by digest, not by path. If two paths share the same digest, one fetch serves both. If a file is renamed, cached content is still valid.

### Prefetch

The client can prefetch content for a subtree:

```
prefetch(glob) -> prefetches matching files:
  SELECT path, digest, size FROM hub_tree
  WHERE path GLOB ? AND digest NOT IN (SELECT digest FROM content_cache)
```

Useful for "make this folder available offline."

### Eviction

LRU eviction based on `accessed_at`:

```
evict(target_bytes):
  current = SELECT SUM(size) FROM content_cache
  if current <= target_bytes -> done

  DELETE FROM content_cache
  WHERE digest IN (
    SELECT digest FROM content_cache
    ORDER BY accessed_at ASC
    LIMIT (select enough rows to free space)
  )
```

The app can set a cache budget:

```
INSERT OR REPLACE INTO sync_state (key, value)
VALUES ('cache_budget', '104857600');  -- 100MB
```

The client checks the budget after each fetch and evicts if over.

### Pinning

Some content should never be evicted:

```sql
CREATE TABLE pinned (
  digest TEXT PRIMARY KEY,
  pinned_at INTEGER NOT NULL
);
```

Pinned digests are excluded from eviction. The app manages pins directly:

```sql
-- Pin everything in a folder
INSERT OR IGNORE INTO pinned (digest, pinned_at)
SELECT digest, unixepoch() FROM hub_tree WHERE path GLOB 'critical/*';

-- Unpin
DELETE FROM pinned WHERE digest IN
  (SELECT digest FROM hub_tree WHERE path GLOB 'critical/*');
```

## App Query Patterns

The app queries `hub_tree` directly. The client library doesn't wrap these — SQL is the API.

### List a directory

```sql
-- Immediate children of 'docs/'
SELECT path, kind, size, mtime FROM hub_tree
WHERE path GLOB 'docs/*' AND path NOT GLOB 'docs/*/*';
```

### Recursive listing

```sql
SELECT path, size FROM hub_tree
WHERE path GLOB 'src/**' AND kind = 0;
```

### Search by name

```sql
SELECT path FROM hub_tree
WHERE path LIKE '%config%' AND kind = 0;
```

### Check what's cached

```sql
SELECT t.path, t.size,
  CASE WHEN c.digest IS NOT NULL THEN 1 ELSE 0 END AS cached
FROM hub_tree t
LEFT JOIN content_cache c ON t.digest = c.digest
WHERE t.path GLOB 'assets/*';
```

### Cache stats

```sql
SELECT
  COUNT(*) AS cached_files,
  SUM(size) AS cached_bytes,
  (SELECT COUNT(*) FROM hub_tree WHERE kind = 0) AS total_files,
  (SELECT SUM(size) FROM hub_tree WHERE kind = 0) AS total_bytes
FROM content_cache;
```

## Rust API Surface

```rust
pub struct HubSyncClient {
    db: rusqlite::Connection,
    hub_url: String,
    token: Option<String>,
    cache_budget: u64,
}

impl HubSyncClient {
    /// Open or create the sync database.
    pub fn open(db_path: &str, hub_url: &str, token: Option<&str>) -> Result<Self>;

    /// Connect to hub and sync tree metadata. Blocks until cancelled.
    pub async fn sync(&self, cancel: CancellationToken) -> Result<()>;

    /// Read file content. Fetches from hub if not cached.
    pub async fn read(&self, path: &str) -> Result<Vec<u8>>;

    /// Prefetch content for files matching a glob pattern.
    pub async fn prefetch(&self, glob: &str) -> Result<u64>; // returns bytes fetched

    /// Evict cached content to stay within budget.
    pub fn evict(&self, target_bytes: u64) -> Result<u64>; // returns bytes freed

    /// Pin content so it won't be evicted.
    pub fn pin(&self, glob: &str) -> Result<u64>; // returns files pinned

    /// Unpin content.
    pub fn unpin(&self, glob: &str) -> Result<u64>;

    /// Get a read-only handle to the database for direct queries.
    pub fn db(&self) -> &rusqlite::Connection;

    /// Set cache budget in bytes.
    pub fn set_cache_budget(&self, bytes: u64) -> Result<()>;
}
```

### FFI (C-compatible)

For iOS/Swift integration:

```c
typedef struct HubSyncClient HubSyncClient;

HubSyncClient* hubsync_open(const char* db_path, const char* hub_url, const char* token);
void hubsync_free(HubSyncClient* client);

// Start sync in background. Returns immediately.
void hubsync_start_sync(HubSyncClient* client);
void hubsync_stop_sync(HubSyncClient* client);

// Read file content. Caller must free the buffer.
int hubsync_read(HubSyncClient* client, const char* path, uint8_t** out_data, size_t* out_len);
void hubsync_free_data(uint8_t* data);

// Cache management
int hubsync_prefetch(HubSyncClient* client, const char* glob);
int hubsync_evict(HubSyncClient* client, uint64_t target_bytes);
int hubsync_pin(HubSyncClient* client, const char* glob);
int hubsync_unpin(HubSyncClient* client, const char* glob);

// Get database path for direct SQLite queries
const char* hubsync_db_path(HubSyncClient* client);
```

Swift code opens a second read-only SQLite connection to the same database for queries:

```swift
let client = HubSyncClient(dbPath: dbPath, hubURL: hubURL, token: token)
client.startSync()

// Direct SQL queries via GRDB or similar
let dbQueue = try DatabaseQueue(path: dbPath, configuration: .readonly)
let files = try dbQueue.read { db in
    try Row.fetchAll(db, sql: "SELECT path, size FROM hub_tree WHERE path GLOB ?", arguments: ["images/*"])
}

// On-demand content fetch
let data = try client.read("images/logo.png")
```

## iOS FileProvider Mapping

The hubsync client maps naturally to iOS FileProvider concepts:

| FileProvider | HubSync |
|---|---|
| `enumerateItems` | `SELECT FROM hub_tree WHERE path GLOB ?` |
| `fetchContents` | `client.read(path)` — fetches on demand |
| `materializedSet` | `content_cache` table (joined with `hub_tree`) |
| `evictItem` | `DELETE FROM content_cache WHERE digest = ?` |
| `setFavoriteRank` / keep downloaded | `pinned` table |
| `currentSyncAnchor` | `sync_state.hub_version` |
| `enumerateChanges(from:)` | `GET /sync/subscribe?since=N` |

The FileProvider extension would wrap the Rust client via FFI, translating between FileProvider protocol and hubsync operations.

## Delta Transfer

For large files that the client has a cached old version of, the client uses rsync delta transfer (same as the Go client):

```
read(path) where old digest cached:
  1. Read old content from content_cache
  2. Compute block signatures (weak rolling checksum + SHA-256)
  3. POST /blobs/delta with signatures
  4. Apply delta ops to reconstruct new content
  5. Verify SHA-256 matches target digest
  6. Update content_cache with new content
```

Falls back to full fetch on failure.

## What's Deferred

- Write support (push changes back to hub)
- Filesystem materialization (writing files to disk, for non-iOS use)
- Background fetch scheduling (iOS BGTaskScheduler integration)
- Conflict detection (for future read-write mode)
- Thumbnail / preview generation
- NSFileProviderItem metadata (UTType, tags, etc.)
