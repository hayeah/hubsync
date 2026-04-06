import Foundation
import GRDB
import SwiftProtobuf

/// Connects to a hubsync hub server and streams tree changes into SQLite.
public final class HubSyncStream {
    private let hubURL: String
    private let token: String?
    private let dbPool: DatabasePool
    private var task: Task<Void, Never>?

    /// Callback fired after each batch of events is applied.
    public var onSync: (() -> Void)?

    public init(hubURL: String, token: String? = nil, dbPool: DatabasePool) {
        self.hubURL = hubURL.hasSuffix("/") ? String(hubURL.dropLast()) : hubURL
        self.token = token
        self.dbPool = dbPool
    }

    /// Start syncing in the background. Returns immediately.
    public func start() {
        task = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                do {
                    try await self.subscribe()
                } catch {
                    if Task.isCancelled { return }
                    print("[HubSync] subscribe error: \(error), retrying in 2s...")
                    try? await Task.sleep(nanoseconds: 2_000_000_000)
                }
            }
        }
    }

    /// Stop syncing.
    public func stop() {
        task?.cancel()
        task = nil
    }

    private func subscribe() async throws {
        let since = try currentVersion()
        let urlString = "\(hubURL)/sync/subscribe?since=\(since)"
        guard let url = URL(string: urlString) else {
            throw HubSyncError.invalidURL(urlString)
        }

        var request = URLRequest(url: url)
        request.timeoutInterval = .infinity
        if let token {
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }

        let (stream, response) = try await URLSession.shared.bytes(for: request)

        guard let httpResponse = response as? HTTPURLResponse,
              httpResponse.statusCode == 200 else {
            let status = (response as? HTTPURLResponse)?.statusCode ?? -1
            throw HubSyncError.httpError(status)
        }

        // Read length-prefixed protobuf messages
        var iterator = stream.makeAsyncIterator()

        while !Task.isCancelled {
            // Read 4-byte big-endian length
            guard let b0 = try await iterator.next(),
                  let b1 = try await iterator.next(),
                  let b2 = try await iterator.next(),
                  let b3 = try await iterator.next() else {
                break // stream ended
            }

            let length = (UInt32(b0) << 24) | (UInt32(b1) << 16) | (UInt32(b2) << 8) | UInt32(b3)

            // Read protobuf payload
            var payload = Data(capacity: Int(length))
            for _ in 0..<length {
                guard let byte = try await iterator.next() else {
                    throw HubSyncError.unexpectedEOF
                }
                payload.append(byte)
            }

            let event = try Hubsync_SyncEvent(serializedBytes: payload)
            try applyEvent(event)
            onSync?()
        }
    }

    private func applyEvent(_ event: Hubsync_SyncEvent) throws {
        try dbPool.write { db in
            let version = Int64(event.version)
            let path = event.path

            switch event.event {
            case .change(let change):
                let digestHex = change.digest.map { String(format: "%02x", $0) }.joined()
                try db.execute(sql: """
                    INSERT OR REPLACE INTO hub_tree (path, kind, digest, size, mode, mtime, version)
                    VALUES (?, ?, ?, ?, ?, ?, ?)
                    """, arguments: [
                        path,
                        Int(change.kind.rawValue),
                        change.digest.isEmpty ? nil : digestHex,
                        Int64(change.size),
                        Int(change.mode),
                        change.mtime,
                        version,
                    ])

                // Cache inline content if provided
                if !change.data.isEmpty {
                    let now = Int64(Date().timeIntervalSince1970)
                    try db.execute(sql: """
                        INSERT OR REPLACE INTO content_cache (digest, data, size, fetched_at, accessed_at)
                        VALUES (?, ?, ?, ?, ?)
                        """, arguments: [
                            digestHex,
                            change.data,
                            Int64(change.data.count),
                            now,
                            now,
                        ])
                }

            case .delete(_):
                // Record deletion for FileProvider change enumeration
                try db.execute(sql: """
                    INSERT OR REPLACE INTO hub_tree_deletions (path, version) VALUES (?, ?)
                    """, arguments: [path, version])
                try db.execute(sql: "DELETE FROM hub_tree WHERE path = ?", arguments: [path])

            case nil:
                break
            }

            // Update sync version
            try db.execute(sql: """
                INSERT OR REPLACE INTO sync_state (key, value) VALUES ('hub_version', ?)
                """, arguments: ["\(version)"])
        }
    }

    private func currentVersion() throws -> Int64 {
        try dbPool.read { db in
            let row = try Row.fetchOne(db, sql: "SELECT value FROM sync_state WHERE key = 'hub_version'")
            return row?["value"] as? Int64 ?? Int64(row?["value"] as? String ?? "0") ?? 0
        }
    }
}

public enum HubSyncError: Error {
    case invalidURL(String)
    case httpError(Int)
    case unexpectedEOF
}
