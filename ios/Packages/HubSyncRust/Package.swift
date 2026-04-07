// swift-tools-version: 6.0
import PackageDescription

// CHubSync is shipped as a binary xcframework that bundles both the
// iOS-device (aarch64-apple-ios) and iOS-simulator (aarch64-apple-ios-sim)
// slices of libhubsync_client.a built from ../../rust-client.
//
// Rebuild with: cd ../.. && pymake xcframework  (in hubsync/ios)
// The xcframework is gitignored — fresh checkouts must regenerate it.
let package = Package(
    name: "HubSyncRust",
    platforms: [.iOS(.v17)],
    products: [
        .library(name: "HubSyncRust", targets: ["HubSyncRust"]),
    ],
    targets: [
        .binaryTarget(
            name: "CHubSync",
            path: "HubSyncClient.xcframework"
        ),
        .target(
            name: "HubSyncRust",
            dependencies: ["CHubSync"],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
