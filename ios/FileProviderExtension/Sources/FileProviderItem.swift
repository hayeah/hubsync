import FileProvider
import UniformTypeIdentifiers
import HubSyncClient

final class HubSyncFileProviderItem: NSObject, NSFileProviderItem {
    let row: HubTreeRow
    let cached: Bool

    init(row: HubTreeRow, cached: Bool = false) {
        self.row = row
        self.cached = cached
    }

    var itemIdentifier: NSFileProviderItemIdentifier {
        NSFileProviderItemIdentifier(row.path)
    }

    var parentItemIdentifier: NSFileProviderItemIdentifier {
        let parent = (row.path as NSString).deletingLastPathComponent
        if parent.isEmpty || parent == "." {
            return .rootContainer
        }
        return NSFileProviderItemIdentifier(parent)
    }

    var filename: String {
        (row.path as NSString).lastPathComponent
    }

    var contentType: UTType {
        if row.kind == 1 {
            return .folder
        }
        let ext = (row.path as NSString).pathExtension
        return UTType(filenameExtension: ext) ?? .data
    }

    var capabilities: NSFileProviderItemCapabilities {
        [.allowsReading]
    }

    var documentSize: NSNumber? {
        row.size.map { NSNumber(value: $0) }
    }

    var contentModificationDate: Date? {
        row.mtime.map { Date(timeIntervalSince1970: TimeInterval($0)) }
    }

    var itemVersion: NSFileProviderItemVersion {
        let content = (row.digest ?? "").data(using: .utf8) ?? Data()
        return NSFileProviderItemVersion(contentVersion: content, metadataVersion: content)
    }

    var isDownloaded: Bool {
        cached
    }
}
