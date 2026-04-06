// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "HubSyncClient",
    platforms: [.iOS(.v17)],
    products: [
        .library(name: "HubSyncClient", targets: ["HubSyncClient"]),
    ],
    dependencies: [
        .package(url: "https://github.com/groue/GRDB.swift", from: "7.0.0"),
        .package(url: "https://github.com/apple/swift-protobuf", from: "1.28.0"),
    ],
    targets: [
        .target(
            name: "HubSyncClient",
            dependencies: [
                .product(name: "GRDB", package: "GRDB.swift"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
            ],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
