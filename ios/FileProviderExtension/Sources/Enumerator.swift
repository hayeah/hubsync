import FileProvider
import HubSyncClient

final class HubSyncEnumerator: NSObject, NSFileProviderEnumerator {
    let client: HubSyncClient
    let containerItemIdentifier: NSFileProviderItemIdentifier

    init(client: HubSyncClient, containerItemIdentifier: NSFileProviderItemIdentifier) {
        self.client = client
        self.containerItemIdentifier = containerItemIdentifier
    }

    func invalidate() {}

    func enumerateItems(for observer: any NSFileProviderEnumerationObserver, startingAt page: NSFileProviderPage) {
        do {
            let parentPath: String
            switch containerItemIdentifier {
            case .rootContainer:
                parentPath = ""
            case .workingSet:
                // Working set: return nothing for now
                observer.finishEnumerating(upTo: nil)
                return
            default:
                parentPath = containerItemIdentifier.rawValue
            }

            let rows = try client.children(of: parentPath)
            let items = rows.map { row in
                let cached = (try? row.digest.map { try client.isCached(digest: $0) }) ?? false
                return HubSyncFileProviderItem(row: row, cached: cached)
            }

            observer.didEnumerate(items)
            observer.finishEnumerating(upTo: nil)
        } catch {
            observer.finishEnumeratingWithError(error)
        }
    }

    func enumerateChanges(for observer: any NSFileProviderChangeObserver, from syncAnchor: NSFileProviderSyncAnchor) {
        let version = Int64(String(data: syncAnchor.rawValue, encoding: .utf8) ?? "0") ?? 0

        do {
            let changed = try client.changes(since: version)
            let changedItems: [NSFileProviderItem] = changed.map { row in
                let cached = (try? row.digest.map { try client.isCached(digest: $0) }) ?? false
                return HubSyncFileProviderItem(row: row, cached: cached)
            }
            observer.didUpdate(changedItems)

            let deleted = try client.deletions(since: version)
            let deletedIDs = deleted.map { NSFileProviderItemIdentifier($0.path) }
            observer.didDeleteItems(withIdentifiers: deletedIDs)

            let currentVersion = try client.hubVersion()
            let anchor = NSFileProviderSyncAnchor(
                "\(currentVersion)".data(using: .utf8)!
            )
            observer.finishEnumeratingChanges(upTo: anchor, moreComing: false)
        } catch {
            observer.finishEnumeratingWithError(error)
        }
    }

    func currentSyncAnchor(completionHandler: @escaping (NSFileProviderSyncAnchor?) -> Void) {
        let version = (try? client.hubVersion()) ?? 0
        let anchor = NSFileProviderSyncAnchor("\(version)".data(using: .utf8)!)
        completionHandler(anchor)
    }
}
