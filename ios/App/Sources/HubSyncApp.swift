import SwiftUI
import FileProvider
import HubSyncClient

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

    private var client: HubSyncClient?
    private var syncStream: HubSyncStream?

    func setup() {
        guard let containerURL = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.com.hubsync.app"
        ) else {
            print("No app group container")
            return
        }

        let dbPath = containerURL.appendingPathComponent("hubsync.db").path
        do {
            client = try HubSyncClient(dbPath: dbPath)
            reload()
        } catch {
            print("DB error: \(error)")
        }
    }

    func reload() {
        guard let client else { return }
        do {
            files = try client.dbPool.read { db in
                try HubTreeRow.fetchAll(db, sql: "SELECT * FROM hub_tree ORDER BY kind DESC, path ASC")
            }
            fileCount = files.count
        } catch {
            print("Failed to load files: \(error)")
        }
    }

    /// Connect to a hub server and start syncing.
    func connectHub(url: String) -> String {
        guard let client else { return "no client" }

        // Stop existing stream
        syncStream?.stop()

        let stream = HubSyncStream(hubURL: url, dbPool: client.dbPool)
        stream.onSync = { [weak self] in
            DispatchQueue.main.async {
                self?.reload()
            }
        }
        stream.start()
        syncStream = stream
        syncStatus = "syncing"
        return "connected to \(url)"
    }

    func addFile(path: String, size: Int) -> String {
        guard let client else { return "no client" }
        do {
            try client.addFile(path: path, size: Int64(size))
            reload()
            return "added \(path), count=\(fileCount)"
        } catch {
            return "error: \(error)"
        }
    }

    func removeFile(path: String) -> String {
        guard let client else { return "no client" }
        do {
            let deleted = try client.removeFile(path: path)
            reload()
            return "removed \(path), deleted=\(deleted), count=\(fileCount)"
        } catch {
            return "error: \(error)"
        }
    }

    func clearAll() -> String {
        guard let client else { return "no client" }
        do {
            try client.clearAll()
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
