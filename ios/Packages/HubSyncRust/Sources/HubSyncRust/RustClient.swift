import Foundation
import CHubSync

/// Swift wrapper around the Rust hubsync client FFI.
public final class RustHubSyncClient: @unchecked Sendable {
    private let handle: OpaquePointer
    private var callbackRef: Unmanaged<CallbacksBox>?

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
        stopSync()
        hubsync_free(handle)
    }

    // MARK: - Sync

    /// Start tree sync in a background thread. Returns immediately.
    public func startSync() {
        hubsync_start_sync(handle)
    }

    /// Start sync with a callback fired on the main queue after each event.
    public func startSync(onEvent: @escaping () -> Void) {
        startSync(onBootstrap: { _, _ in }, onEvent: onEvent)
    }

    /// Start sync with separate bootstrap and event callbacks.
    /// `onBootstrap(count, total)` — (0, N) means tree import complete with N entries;
    /// (count, total) reports blob fetch progress.
    /// `onEvent` fires for each incremental sync event.
    /// Both callbacks are dispatched to the main queue.
    public func startSync(
        onBootstrap: @escaping (UInt64, UInt64) -> Void,
        onEvent: @escaping () -> Void
    ) {
        releaseCallback()

        let box = CallbacksBox(
            onBootstrap: { count, total in
                DispatchQueue.main.async { onBootstrap(count, total) }
            },
            onEvent: {
                DispatchQueue.main.async { onEvent() }
            }
        )
        let ref = Unmanaged.passRetained(box)
        callbackRef = ref
        let ctx = ref.toOpaque()
        hubsync_start_sync_with_callbacks(
            handle,
            { count, total, ctx in
                guard let ctx else { return }
                Unmanaged<CallbacksBox>.fromOpaque(ctx).takeUnretainedValue().onBootstrap(count, total)
            },
            { ctx in
                guard let ctx else { return }
                Unmanaged<CallbacksBox>.fromOpaque(ctx).takeUnretainedValue().onEvent()
            },
            ctx
        )
    }

    /// Stop the background sync thread.
    public func stopSync() {
        hubsync_stop_sync(handle)
        releaseCallback()
    }

    private func releaseCallback() {
        callbackRef?.release()
        callbackRef = nil
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

private class CallbacksBox {
    let onBootstrap: (UInt64, UInt64) -> Void
    let onEvent: () -> Void
    init(onBootstrap: @escaping (UInt64, UInt64) -> Void, onEvent: @escaping () -> Void) {
        self.onBootstrap = onBootstrap
        self.onEvent = onEvent
    }
}
