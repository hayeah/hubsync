"""HubSync iOS build pipeline.

Builds the Rust static library for iOS targets, generates the Xcode project,
and builds the iOS app.

Run with: pymake
List tasks: pymake list
"""

from pathlib import Path

from pymake import sh, task

# Paths
RUST_CLIENT = Path("../rust-client")
RUST_LIB_SIM = RUST_CLIENT / "target/aarch64-apple-ios-sim/release/libhubsync_client.a"
RUST_LIB_IOS = RUST_CLIENT / "target/aarch64-apple-ios/release/libhubsync_client.a"
RUST_HEADER = RUST_CLIENT / "include/hubsync.h"
XCODEPROJ = Path("HubSync.xcodeproj")

# Cargo needs PATH to find rustup toolchain
CARGO_ENV = "PATH=$HOME/.cargo/bin:$PATH"


@task(
    inputs=[*RUST_CLIENT.glob("src/**/*.rs"), RUST_CLIENT / "Cargo.toml"],
    outputs=[RUST_LIB_SIM],
)
def rust_sim():
    """Build Rust static library for iOS simulator (arm64)."""
    sh(f"cd {RUST_CLIENT} && {CARGO_ENV} cargo build --release --target aarch64-apple-ios-sim")


@task(
    inputs=[*RUST_CLIENT.glob("src/**/*.rs"), RUST_CLIENT / "Cargo.toml"],
    outputs=[RUST_LIB_IOS],
)
def rust_ios():
    """Build Rust static library for iOS device (arm64)."""
    sh(f"cd {RUST_CLIENT} && {CARGO_ENV} cargo build --release --target aarch64-apple-ios")


@task(
    inputs=[Path("project.yml")],
    outputs=[XCODEPROJ / "project.pbxproj"],
)
def xcodegen():
    """Generate Xcode project from project.yml."""
    sh("xcodegen generate")


@task(inputs=[rust_sim, xcodegen])
def build_sim():
    """Build iOS app for simulator."""
    sh(
        "xcodebuild -project HubSync.xcodeproj -scheme HubSync"
        " -destination 'platform=iOS Simulator,name=iPhone 17 Pro'"
        " -skipMacroValidation build"
    )


@task(inputs=[rust_ios, xcodegen])
def build_device():
    """Build iOS app for device."""
    sh(
        "xcodebuild -project HubSync.xcodeproj -scheme HubSync"
        " -destination 'generic/platform=iOS'"
        " -skipMacroValidation build"
    )


@task(inputs=[build_sim])
def install():
    """Install app on booted simulator."""
    import glob as g
    apps = g.glob(str(Path.home() / "Library/Developer/Xcode/DerivedData/HubSync-*/Build/Products/Debug-iphonesimulator/HubSync.app"))
    if not apps:
        raise RuntimeError("HubSync.app not found in DerivedData")
    app = apps[0]
    sh(f"xcrun simctl install booted '{app}'")
    sh("xcrun simctl launch booted com.hubsync.app")


@task()
def proto():
    """Regenerate Swift protobuf from hubsync.proto."""
    sh(
        "protoc -I../rust-client/proto"
        " --swift_out=Packages/HubSyncClient/Sources/HubSyncClient/Proto"
        " --swift_opt=Visibility=Public"
        " hubsync.proto"
    )


task.default(build_sim)
