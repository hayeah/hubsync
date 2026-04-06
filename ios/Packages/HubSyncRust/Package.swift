// swift-tools-version: 6.0
import PackageDescription
import Foundation

// Resolve absolute path: Package.swift is at ios/Packages/HubSyncRust/
// rust-client is at <repo>/rust-client, so 3 levels up
let packageDir = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
let rustClientDir = packageDir
    .appendingPathComponent("../../../rust-client")
    .standardized.path

let package = Package(
    name: "HubSyncRust",
    platforms: [.iOS(.v17)],
    products: [
        .library(name: "HubSyncRust", targets: ["HubSyncRust"]),
    ],
    targets: [
        .target(
            name: "CHubSync",
            publicHeadersPath: "include",
            linkerSettings: [
                .unsafeFlags([
                    "-L\(rustClientDir)/target/aarch64-apple-ios-sim/release",
                    "-L\(rustClientDir)/target/aarch64-apple-ios/release",
                ]),
                .linkedLibrary("hubsync_client"),
            ]
        ),
        .target(
            name: "HubSyncRust",
            dependencies: ["CHubSync"],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
