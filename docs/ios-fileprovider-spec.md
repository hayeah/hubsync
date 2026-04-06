# iOS FileProvider Extension for HubSync

An iOS FileProvider extension that exposes hubsync-synced files as a native file provider domain. Wraps the Rust client via FFI, translating between the FileProvider protocol and hubsync operations.

## FileProvider → HubSync Mapping

| FileProvider | HubSync |
|---|---|
| `enumerateItems` | `SELECT FROM hub_tree WHERE path GLOB ?` |
| `fetchContents` | `client.read(path)` — fetches on demand |
| `materializedSet` | `content_cache` table (joined with `hub_tree`) |
| `evictItem` | `DELETE FROM content_cache WHERE digest = ?` |
| `setFavoriteRank` / keep downloaded | `pinned` table |
| `currentSyncAnchor` | `sync_state.hub_version` |
| `enumerateChanges(from:)` | `GET /sync/subscribe?since=N` |

## Architecture

```
┌─────────────────────────────────┐
│  iOS App                        │
│  ┌───────────────────────────┐  │
│  │ FileProvider Extension    │  │
│  │  ┌─────────────────────┐  │  │
│  │  │ Swift FileProvider   │  │  │
│  │  │ protocol impl       │  │  │
│  │  └────────┬────────────┘  │  │
│  │           │ FFI           │  │
│  │  ┌────────▼────────────┐  │  │
│  │  │ HubSync Rust Client │  │  │
│  │  │ (static library)    │  │  │
│  │  └────────┬────────────┘  │  │
│  │           │               │  │
│  │  ┌────────▼────────────┐  │  │
│  │  │ SQLite database     │  │  │
│  │  └─────────────────────┘  │  │
│  └───────────────────────────┘  │
└─────────────────────────────────┘
```

## Item Identifiers

FileProvider requires stable `NSFileProviderItemIdentifier` values. Use the `hub_tree.path` as the identifier, URL-encoded to be safe as an opaque string.

- Root container: maps to `""` (empty path prefix)
- Working set: all pinned items + recently accessed items

## Enumerating Items

### `enumerateItems(for:startingAt:)`

```swift
func enumerateItems(for containerItemIdentifier: NSFileProviderItemIdentifier,
                    startingAt page: NSFileProviderPage) -> [NSFileProviderItem] {
    let parentPath = containerItemIdentifier.rawValue
    let pattern = parentPath.isEmpty ? "*" : "\(parentPath)/*"

    // Direct children only (no recursive descent)
    let rows = db.query("""
        SELECT path, kind, size, mtime, digest FROM hub_tree
        WHERE path GLOB ? AND path NOT GLOB ?
        """, [pattern, parentPath.isEmpty ? "*/*" : "\(parentPath)/*/*"])

    return rows.map { HubSyncFileProviderItem(row: $0) }
}
```

### `enumerateChanges(from:)`

```swift
func enumerateChanges(from syncAnchor: NSFileProviderSyncAnchor) -> NSFileProviderChangeSet {
    let oldVersion = Int(syncAnchor)
    let currentVersion = client.hubVersion()

    // The Rust client applies events from /sync/subscribe to hub_tree.
    // Diff is computed by comparing version numbers on hub_tree rows.
    let changed = db.query("""
        SELECT path, kind, size, mtime, digest FROM hub_tree
        WHERE version > ?
        """, [oldVersion])

    // Deleted items tracked via a deletion log or tombstone table
    let deleted = db.query("""
        SELECT path FROM hub_tree_deletions
        WHERE version > ?
        """, [oldVersion])

    return ChangeSet(
        updated: changed.map { HubSyncFileProviderItem(row: $0) },
        deleted: deleted.map { NSFileProviderItemIdentifier($0.path) },
        newAnchor: NSFileProviderSyncAnchor(currentVersion)
    )
}
```

**Deletion tracking**: the Rust client needs a `hub_tree_deletions` table to record paths removed by `FileDelete` events, so the FileProvider can report them in `enumerateChanges`. Periodically prune old deletions.

```sql
CREATE TABLE hub_tree_deletions (
  path    TEXT PRIMARY KEY,
  version INTEGER NOT NULL
);
```

## Fetching Content

### `fetchContents(for:version:)`

```swift
func fetchContents(for itemIdentifier: NSFileProviderItemIdentifier,
                   version: NSFileProviderItemVersion?,
                   request: NSFileProviderRequest,
                   completionHandler: @escaping (URL?, NSFileProviderItem?, Error?) -> Void) {
    let path = itemIdentifier.rawValue

    DispatchQueue.global().async {
        do {
            let data = try self.client.read(path)  // FFI call — fetches from hub if not cached

            // Write to a temp file for FileProvider to pick up
            let tempURL = NSFileProviderManager.default.documentStorageURL
                .appendingPathComponent(UUID().uuidString)
            try data.write(to: tempURL)

            let item = HubSyncFileProviderItem(path: path, db: self.db)
            completionHandler(tempURL, item, nil)
        } catch {
            completionHandler(nil, nil, error)
        }
    }
}
```

## Materialized Set & Eviction

The system tracks which items are "materialized" (content available locally). This maps directly to the `content_cache` table.

### `materializedItemsDidChange(_:)`

No special handling needed — the Rust client's `content_cache` already tracks what's cached.

### `evictItem(identifier:)`

```swift
func evictItem(identifier: NSFileProviderItemIdentifier,
               completionHandler: @escaping (Error?) -> Void) {
    let path = identifier.rawValue

    do {
        let digest = db.query("SELECT digest FROM hub_tree WHERE path = ?", [path])
        if let digest = digest {
            db.execute("DELETE FROM content_cache WHERE digest = ?", [digest])
        }
        completionHandler(nil)
    } catch {
        completionHandler(error)
    }
}
```

## Pinning (Keep Downloaded)

Users can mark items as "keep downloaded" in Files.app. Map this to the `pinned` table.

```swift
func setFavoriteRank(_ favoriteRank: NSNumber?, forItemIdentifier: NSFileProviderItemIdentifier) {
    // favoriteRank != nil means "keep downloaded"
    let path = forItemIdentifier.rawValue

    if favoriteRank != nil {
        client.pin(path)       // FFI call
        client.prefetch(path)  // ensure content is fetched
    } else {
        client.unpin(path)
    }
}
```

## NSFileProviderItem Implementation

```swift
class HubSyncFileProviderItem: NSObject, NSFileProviderItem {
    let path: String
    let kind: Int
    let size: Int64?
    let mtime: Date?
    let digest: String?
    let isCached: Bool

    var itemIdentifier: NSFileProviderItemIdentifier {
        NSFileProviderItemIdentifier(path)
    }

    var parentItemIdentifier: NSFileProviderItemIdentifier {
        let parent = (path as NSString).deletingLastPathComponent
        return parent.isEmpty
            ? .rootContainer
            : NSFileProviderItemIdentifier(parent)
    }

    var filename: String {
        (path as NSString).lastPathComponent
    }

    var contentType: UTType {
        kind == 1 ? .folder : UTType(filenameExtension: (path as NSString).pathExtension) ?? .data
    }

    var documentSize: NSNumber? {
        size.map { NSNumber(value: $0) }
    }

    var contentModificationDate: Date? {
        mtime
    }

    var itemVersion: NSFileProviderItemVersion {
        // Use digest as the content version
        let contentVersion = (digest ?? "").data(using: .utf8) ?? Data()
        return NSFileProviderItemVersion(contentVersion: contentVersion, metadataVersion: contentVersion)
    }

    var isDownloaded: Bool {
        isCached
    }
}
```

## Lifecycle

- **Extension launch**: open the Rust client (`hubsync_open`), start tree sync (`hubsync_start_sync`)
- **Extension suspension**: stop sync (`hubsync_stop_sync`), but keep the database — it persists across launches
- **Background refresh**: use `NSFileProviderManager.signalEnumerator` when the Rust client receives new tree events to wake the extension

## What's Deferred

- Write support (uploading changes back to hub)
- Background fetch scheduling (iOS BGTaskScheduler integration)
- Thumbnail / preview generation via `NSFileProviderThumbnailing`
- `NSFileProviderItem` metadata enrichment (UTType detection, tags, etc.)
- Conflict resolution UI (for future read-write mode)
- Progress reporting for large fetches via `NSProgress`
