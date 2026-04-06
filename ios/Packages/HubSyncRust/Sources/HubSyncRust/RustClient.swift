import Foundation
import CHubSync

/// Swift wrapper around the Rust hubsync client FFI.
public final class RustHubSyncClient: @unchecked Sendable {
    private let handle: OpaquePointer

    /// Open a hubsync client.
    /// - Parameters:
    ///   - dbPath: Path to SQLite database (created if needed).
    ///   - hubURL: Hub server URL (e.g. "http://localhost:8080").
    ///   - token: Bearer token for auth, or nil for no auth.
    public init?(dbPath: String, hubURL: String, token: String? = nil) {
        guard let h = dbPath.withCString({ dbPath in
            hubURL.withCString({ hubURL in
                if let token {
                    return token.withCString { token in
                        hubsync_open(dbPath, hubURL, token)
                    }
                } else {
                    return hubsync_open(dbPath, hubURL, nil)
                }
            })
        }) else {
            return nil
        }
        self.handle = h
    }

    deinit {
        hubsync_free(handle)
    }

    // MARK: - Sync

    /// Start tree sync in a background thread. Returns immediately.
    public func startSync() {
        hubsync_start_sync(handle)
    }

    /// Start sync with a callback fired on the main queue after each event.
    public func startSync(onEvent: @escaping () -> Void) {
        // Store callback in a box so we can pass it through C void*
        let box = CallbackBox {
            DispatchQueue.main.async { onEvent() }
        }
        let ctx = Unmanaged.passRetained(box).toOpaque()
        hubsync_start_sync_with_callback(handle, { ctx in
            guard let ctx else { return }
            Unmanaged<CallbackBox>.fromOpaque(ctx).takeUnretainedValue().callback()
        }, ctx)
    }

    /// Stop the background sync thread.
    public func stopSync() {
        hubsync_stop_sync(handle)
    }

    // MARK: - Content

    /// Read file content by path. Fetches from hub if not cached.
    public func read(path: String) -> Data? {
        var dataPtr: UnsafeMutablePointer<UInt8>?
        var len: Int = 0
        let result = path.withCString { path in
            hubsync_read(handle, path, &dataPtr, &len)
        }
        guard result == 0, let ptr = dataPtr else { return nil }
        let data = Data(bytes: ptr, count: len)
        hubsync_free_data(ptr, len)
        return data
    }

    // MARK: - Cache management

    /// Prefetch content for files matching a glob pattern.
    public func prefetch(glob: String) -> Int64 {
        glob.withCString { hubsync_prefetch(handle, $0) }
    }

    /// Evict cached content to stay within target bytes.
    public func evict(targetBytes: UInt64) -> Int64 {
        hubsync_evict(handle, targetBytes)
    }

    /// Pin files matching glob (prevents LRU eviction).
    public func pin(glob: String) -> Int64 {
        glob.withCString { hubsync_pin(handle, $0) }
    }

    /// Unpin files matching glob.
    public func unpin(glob: String) -> Int64 {
        glob.withCString { hubsync_unpin(handle, $0) }
    }

    // MARK: - Queries

    /// Get the current hub sync version.
    public var hubVersion: Int64 {
        hubsync_hub_version(handle)
    }

    /// Get the database path.
    public var dbPath: String? {
        guard let ptr = hubsync_db_path(handle) else { return nil }
        let str = String(cString: ptr)
        hubsync_free_string(ptr)
        return str
    }
}

private class CallbackBox {
    let callback: () -> Void
    init(_ callback: @escaping () -> Void) {
        self.callback = callback
    }
}
