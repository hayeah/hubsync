import SwiftUI
import HubSyncClient
import HubSyncRust

#if DEBUG
import SwiftUITap
#endif

#if DEBUG
@SwiftUITap
#endif
@Observable
final class AppState {
    var files: [HubTreeRow] = []
    var fileCount: Int = 0
    var syncStatus: String = "disconnected"

    var __doc__: String {
        """
        AppState — root state for HubSync iOS app.
        files ([HubTreeRow]) — all files from hub_tree, sorted dirs first
        fileCount (Int) — total number of entries
        syncStatus (String) — "disconnected", "syncing", or error message
        Methods: reload(), connectHub(url:), addFile(path:size:), removeFile(path:), clearAll()
        """
    }

    /// GRDB client for direct SQL queries (file listing, etc.)
    private var grdbClient: HubSyncClient?

    /// Rust FFI client for sync + content fetch
    private var rustClient: RustHubSyncClient?

    private var dbPath: String?

    func setup() {
        guard let containerURL = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.com.hubsync.app"
        ) else {
            print("No app group container")
            return
        }

        let path = containerURL.appendingPathComponent("hubsync.db").path
        dbPath = path
        do {
            grdbClient = try HubSyncClient(dbPath: path)
            reload()
        } catch {
            print("DB error: \(error)")
        }
    }

    func reload() {
        guard let grdbClient else { return }
        do {
            files = try grdbClient.dbPool.read { db in
                try HubTreeRow.fetchAll(db, sql: "SELECT * FROM hub_tree ORDER BY kind DESC, path ASC")
            }
            fileCount = files.count
        } catch {
            print("Failed to load files: \(error)")
        }
    }

    /// Connect to a hub server and start syncing via the Rust client.
    func connectHub(url: String) -> String {
        guard let dbPath else { return "no db path" }

        // Stop existing sync
        rustClient?.stopSync()

        guard let client = RustHubSyncClient(dbPath: dbPath, hubURL: url) else {
            return "failed to open rust client"
        }

        client.startSync(onEvent: { [weak self] in
            self?.reload()
        })

        rustClient = client
        syncStatus = "syncing"
        return "connected to \(url) (rust)"
    }

    func addFile(path: String, size: Int) -> String {
        guard let grdbClient else { return "no client" }
        do {
            try grdbClient.addFile(path: path, size: Int64(size))
            reload()
            return "added \(path), count=\(fileCount)"
        } catch {
            return "error: \(error)"
        }
    }

    func removeFile(path: String) -> String {
        guard let grdbClient else { return "no client" }
        do {
            let deleted = try grdbClient.removeFile(path: path)
            reload()
            return "removed \(path), deleted=\(deleted), count=\(fileCount)"
        } catch {
            return "error: \(error)"
        }
    }

    func clearAll() -> String {
        guard let grdbClient else { return "no client" }
        do {
            try grdbClient.clearAll()
            reload()
            return "cleared, count=\(fileCount)"
        } catch {
            return "error: \(error)"
        }
    }
}

private let sharedAppState = AppState()

@main
struct HubSyncApp: App {
    var body: some Scene {
        WindowGroup {
            FileListView()
                .environment(sharedAppState)
                .tapInspectable()
                .onAppear {
                    sharedAppState.setup()
                    #if DEBUG
                    SwiftUITap.poll(state: sharedAppState, server: "http://localhost:9876")
                    #endif
                }
        }
    }
}

struct FileListView: View {
    @Environment(AppState.self) private var state

    var body: some View {
        NavigationStack {
            List(state.files, id: \.path) { file in
                HStack {
                    Image(systemName: file.kind == 1 ? "folder.fill" : "doc.fill")
                        .foregroundStyle(file.kind == 1 ? .blue : .secondary)
                    VStack(alignment: .leading) {
                        Text(file.path)
                            .font(.body)
                        if let size = file.size {
                            Text(ByteCountFormatter.string(fromByteCount: size, countStyle: .file))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            }
            .navigationTitle("HubSync Files")
        }
    }
}
