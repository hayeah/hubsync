import FileProvider
import HubSyncClient

final class HubSyncFileProviderExtension: NSObject, NSFileProviderReplicatedExtension {
    let domain: NSFileProviderDomain
    let client: HubSyncClient

    required init(domain: NSFileProviderDomain) {
        self.domain = domain

        // Store DB in shared app group container
        let containerURL = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.com.hubsync.app"
        )!
        let dbPath = containerURL.appendingPathComponent("hubsync.db").path
        self.client = try! HubSyncClient(dbPath: dbPath)

        super.init()
    }

    func invalidate() {}

    // MARK: - Enumeration

    func enumerator(
        for containerItemIdentifier: NSFileProviderItemIdentifier,
        request: NSFileProviderRequest
    ) throws -> any NSFileProviderEnumerator {
        HubSyncEnumerator(client: client, containerItemIdentifier: containerItemIdentifier)
    }

    // MARK: - Item lookup

    func item(
        for identifier: NSFileProviderItemIdentifier,
        request: NSFileProviderRequest,
        completionHandler: @escaping (NSFileProviderItem?, Error?) -> Void
    ) -> Progress {
        let progress = Progress(totalUnitCount: 1)
        do {
            let path = identifier.rawValue
            guard let row = try client.treeEntry(for: path) else {
                completionHandler(nil, NSFileProviderError(.noSuchItem))
                return progress
            }
            let cached = (try? row.digest.map { try client.isCached(digest: $0) }) ?? false
            completionHandler(HubSyncFileProviderItem(row: row, cached: cached), nil)
        } catch {
            completionHandler(nil, error)
        }
        progress.completedUnitCount = 1
        return progress
    }

    // MARK: - Content fetch

    func fetchContents(
        for itemIdentifier: NSFileProviderItemIdentifier,
        version requestedVersion: NSFileProviderItemVersion?,
        request: NSFileProviderRequest,
        completionHandler: @escaping (URL?, NSFileProviderItem?, Error?) -> Void
    ) -> Progress {
        let progress = Progress(totalUnitCount: 1)
        let path = itemIdentifier.rawValue

        do {
            guard let row = try client.treeEntry(for: path) else {
                completionHandler(nil, nil, NSFileProviderError(.noSuchItem))
                return progress
            }

            guard let data = try client.readContent(for: path) else {
                // Content not cached — in a real implementation this would
                // fetch from the hub server
                completionHandler(nil, nil, NSFileProviderError(.serverUnreachable))
                return progress
            }

            // Write to temp file for FileProvider
            let tempDir = FileManager.default.temporaryDirectory
            let tempURL = tempDir.appendingPathComponent(UUID().uuidString)
            try data.write(to: tempURL)

            let cached = true
            let item = HubSyncFileProviderItem(row: row, cached: cached)
            completionHandler(tempURL, item, nil)
        } catch {
            completionHandler(nil, nil, error)
        }
        progress.completedUnitCount = 1
        return progress
    }

    // MARK: - Write stubs (read-only for now)

    func createItem(
        basedOn itemTemplate: NSFileProviderItem,
        fields: NSFileProviderItemFields,
        contents url: URL?,
        options: NSFileProviderCreateItemOptions,
        request: NSFileProviderRequest,
        completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, Error?) -> Void
    ) -> Progress {
        completionHandler(nil, [], false, NSError(domain: NSCocoaErrorDomain, code: NSFeatureUnsupportedError))
        return Progress()
    }

    func modifyItem(
        _ item: NSFileProviderItem,
        baseVersion version: NSFileProviderItemVersion,
        changedFields: NSFileProviderItemFields,
        contents newContents: URL?,
        options: NSFileProviderModifyItemOptions,
        request: NSFileProviderRequest,
        completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, Error?) -> Void
    ) -> Progress {
        completionHandler(nil, [], false, NSError(domain: NSCocoaErrorDomain, code: NSFeatureUnsupportedError))
        return Progress()
    }

    func deleteItem(
        identifier: NSFileProviderItemIdentifier,
        baseVersion version: NSFileProviderItemVersion,
        options: NSFileProviderDeleteItemOptions,
        request: NSFileProviderRequest,
        completionHandler: @escaping (Error?) -> Void
    ) -> Progress {
        completionHandler(NSError(domain: NSCocoaErrorDomain, code: NSFeatureUnsupportedError))
        return Progress()
    }
}
