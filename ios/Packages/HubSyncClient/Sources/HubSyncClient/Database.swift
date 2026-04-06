import Foundation
import GRDB

public struct HubTreeRow: Codable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "hub_tree"

    public var path: String
    public var kind: Int        // 0=file, 1=dir, 2=symlink
    public var digest: String?  // hex SHA-256, nil for dirs
    public var size: Int64?
    public var mode: Int?
    public var mtime: Int64?    // unix seconds
    public var version: Int64
}

public struct ContentCacheRow: Codable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "content_cache"

    public var digest: String
    public var data: Data
    public var size: Int64
    public var fetched_at: Int64
    public var accessed_at: Int64
}

public struct HubTreeDeletionRow: Codable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "hub_tree_deletions"

    public var path: String
    public var version: Int64
}

public struct SyncStateRow: Codable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "sync_state"

    public var key: String
    public var value: String
}

public func createSchema(in db: GRDB.Database) throws {
    try db.create(table: "hub_tree", ifNotExists: true) { t in
        t.primaryKey("path", .text)
        t.column("kind", .integer).notNull().defaults(to: 0)
        t.column("digest", .text)
        t.column("size", .integer)
        t.column("mode", .integer)
        t.column("mtime", .integer)
        t.column("version", .integer).notNull()
    }

    try db.create(index: "idx_hub_tree_digest", on: "hub_tree", columns: ["digest"], ifNotExists: true)

    try db.create(table: "content_cache", ifNotExists: true) { t in
        t.primaryKey("digest", .text)
        t.column("data", .blob).notNull()
        t.column("size", .integer).notNull()
        t.column("fetched_at", .integer).notNull()
        t.column("accessed_at", .integer).notNull()
    }

    try db.create(table: "hub_tree_deletions", ifNotExists: true) { t in
        t.primaryKey("path", .text)
        t.column("version", .integer).notNull()
    }

    try db.create(table: "sync_state", ifNotExists: true) { t in
        t.primaryKey("key", .text)
        t.column("value", .text).notNull()
    }
}
