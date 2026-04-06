import Foundation
import GRDB

public final class HubSyncClient: Sendable {
    public let dbPool: DatabasePool

    public init(dbPath: String) throws {
        var config = Configuration()
        config.busyMode = .timeout(5) // 5 second busy timeout for WAL contention with Rust client
        config.readonly = false
        dbPool = try DatabasePool(path: dbPath, configuration: config)
        try dbPool.write { db in
            try createSchema(in: db)
        }
    }

    // MARK: - Tree queries

    /// List immediate children of a directory path.
    public func children(of parentPath: String) throws -> [HubTreeRow] {
        try dbPool.read { db in
            if parentPath.isEmpty {
                return try HubTreeRow.fetchAll(db, sql: """
                    SELECT * FROM hub_tree
                    WHERE path GLOB ? AND path NOT GLOB ?
                    """, arguments: ["*", "*/*"])
            } else {
                return try HubTreeRow.fetchAll(db, sql: """
                    SELECT * FROM hub_tree
                    WHERE path GLOB ? AND path NOT GLOB ?
                    """, arguments: ["\(parentPath)/*", "\(parentPath)/*/*"])
            }
        }
    }

    /// Look up a single tree entry by path.
    public func treeEntry(for path: String) throws -> HubTreeRow? {
        try dbPool.read { db in
            try HubTreeRow.fetchOne(db, key: path)
        }
    }

    // MARK: - Content

    /// Read file content. Returns cached data if available.
    public func readContent(for path: String) throws -> Data? {
        try dbPool.read { db in
            guard let row = try HubTreeRow.fetchOne(db, key: path),
                  let digest = row.digest else {
                return nil
            }
            return try ContentCacheRow.fetchOne(db, key: digest)?.data
        }
    }

    /// Check if content is cached for a given digest.
    public func isCached(digest: String) throws -> Bool {
        try dbPool.read { db in
            try ContentCacheRow.exists(db, key: digest)
        }
    }

    // MARK: - Sync state

    public func hubVersion() throws -> Int64 {
        try dbPool.read { db in
            let row = try SyncStateRow.fetchOne(db, key: "hub_version")
            return Int64(row?.value ?? "0") ?? 0
        }
    }

    /// Get deletions since a given version.
    public func deletions(since version: Int64) throws -> [HubTreeDeletionRow] {
        try dbPool.read { db in
            try HubTreeDeletionRow.fetchAll(db, sql: """
                SELECT * FROM hub_tree_deletions WHERE version > ?
                """, arguments: [version])
        }
    }

    /// Get changed items since a given version.
    public func changes(since version: Int64) throws -> [HubTreeRow] {
        try dbPool.read { db in
            try HubTreeRow.fetchAll(db, sql: """
                SELECT * FROM hub_tree WHERE version > ?
                """, arguments: [version])
        }
    }

    // MARK: - Mutations

    /// Add a file to the tree.
    public func addFile(path: String, size: Int64) throws {
        try dbPool.write { db in
            let now = Int64(Date().timeIntervalSince1970)
            let digest = String(path.hashValue, radix: 16)
            let version = try self.currentVersion(db) + 1
            try HubTreeRow(
                path: path, kind: 0, digest: digest,
                size: size, mode: 0o644, mtime: now, version: version
            ).insert(db)
            try SyncStateRow(key: "hub_version", value: "\(version)").save(db)
        }
    }

    /// Remove a file from the tree.
    public func removeFile(path: String) throws -> Bool {
        try dbPool.write { db in
            try HubTreeRow.deleteOne(db, key: path)
        }
    }

    /// Remove all tree entries and cached content.
    public func clearAll() throws {
        try dbPool.write { db in
            _ = try HubTreeRow.deleteAll(db)
            _ = try ContentCacheRow.deleteAll(db)
            try SyncStateRow(key: "hub_version", value: "0").save(db)
        }
    }

    private func currentVersion(_ db: GRDB.Database) throws -> Int64 {
        let row = try SyncStateRow.fetchOne(db, key: "hub_version")
        return Int64(row?.value ?? "0") ?? 0
    }

    // MARK: - Mock data (for testing)

    /// Seed the database with sample files for testing.
    public func seedMockData() throws {
        try dbPool.write { db in
            let now = Int64(Date().timeIntervalSince1970)
            let version: Int64 = 1

            let dirs: [(String, Int)] = [
                ("docs", 1),
                ("src", 1),
                ("src/components", 1),
            ]
            for (path, kind) in dirs {
                try HubTreeRow(
                    path: path, kind: kind, digest: nil,
                    size: nil, mode: 0o755, mtime: now, version: version
                ).insert(db)
            }

            let files: [(String, String, Int64)] = [
                ("README.md", "abc123", 1024),
                ("docs/guide.md", "def456", 2048),
                ("docs/api.md", "ghi789", 4096),
                ("src/main.swift", "jkl012", 512),
                ("src/components/Button.swift", "mno345", 768),
            ]
            for (path, digest, size) in files {
                try HubTreeRow(
                    path: path, kind: 0, digest: digest,
                    size: size, mode: 0o644, mtime: now, version: version
                ).insert(db)
            }

            // Cache some content
            let sampleContent = "# Hello from HubSync\n\nThis is a mock file.".data(using: .utf8)!
            try ContentCacheRow(
                digest: "abc123", data: sampleContent,
                size: Int64(sampleContent.count), fetched_at: now, accessed_at: now
            ).insert(db)

            try SyncStateRow(key: "hub_version", value: "1").insert(db)
        }
    }
}
