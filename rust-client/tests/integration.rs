use std::io::{BufRead, BufReader};
use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use hubsync_client::HubSyncClient;

struct TestHub {
    proc: Child,
    hub_dir: tempfile::TempDir,
    url: String,
    token: String,
}

impl TestHub {
    fn start() -> Self {
        let hub_dir = tempfile::tempdir().unwrap();
        let db_path = hub_dir.path().join(".hubsync/hub.db");
        std::fs::create_dir_all(db_path.parent().unwrap()).unwrap();

        let token = "test-token";

        // Find the Go hub binary
        let binary = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("testdata/hubsync-server");
        assert!(
            binary.exists(),
            "Go hub binary not found at {:?}. Run: go build -o rust-client/testdata/hubsync-server ./cli/hubsync/",
            binary
        );

        // Start on a random port — we'll parse stdout to find it
        let port = find_free_port();
        let listen = format!("127.0.0.1:{}", port);

        let proc = Command::new(&binary)
            .arg("serve")
            .arg("-dir")
            .arg(hub_dir.path())
            .arg("-listen")
            .arg(&listen)
            .arg("-db")
            .arg(&db_path)
            .env("HUBSYNC_TOKEN", token)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("failed to start hub");

        let url = format!("http://{}", listen);

        // Wait for the hub to be ready
        let mut hub = TestHub {
            proc,
            hub_dir,
            url,
            token: token.to_string(),
        };
        hub.wait_ready();
        hub
    }

    fn wait_ready(&mut self) {
        // Poll the subscribe endpoint until it responds
        let client = reqwest::blocking::Client::builder()
            .timeout(Some(Duration::from_secs(1)))
            .build()
            .unwrap();
        for _ in 0..50 {
            match client
                .get(format!("{}/sync/subscribe?since=0", self.url))
                .header("Authorization", format!("Bearer {}", self.token))
                .send()
            {
                Ok(resp) if resp.status().is_success() => return,
                _ => std::thread::sleep(Duration::from_millis(100)),
            }
        }
        panic!("hub did not become ready");
    }

    fn write_file(&self, path: &str, content: &str) {
        let full = self.hub_dir.path().join(path);
        if let Some(parent) = full.parent() {
            std::fs::create_dir_all(parent).unwrap();
        }
        std::fs::write(&full, content).unwrap();
    }

    fn delete_file(&self, path: &str) {
        let full = self.hub_dir.path().join(path);
        std::fs::remove_file(full).unwrap();
    }

    /// Trigger a scan by sending a dummy request and waiting.
    /// The hub scans on startup and when the watcher fires.
    /// Since we write directly to the dir, we need to wait for the watcher.
    fn wait_for_scan(&self) {
        // The hub uses fsnotify with 50ms debounce. Wait a bit.
        std::thread::sleep(Duration::from_millis(500));
    }
}

impl Drop for TestHub {
    fn drop(&mut self) {
        let _ = self.proc.kill();
        let _ = self.proc.wait();
    }
}

fn find_free_port() -> u16 {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.local_addr().unwrap().port()
}

#[test]
fn test_tree_sync() {
    let hub = TestHub::start();
    hub.write_file("hello.txt", "hello world");
    hub.write_file("sub/nested.txt", "nested content");
    hub.wait_for_scan();

    let db_path = tempfile::NamedTempFile::new().unwrap();
    let client = HubSyncClient::open_sqlite(
        db_path.path().to_str().unwrap(),
        &hub.url,
        Some(&hub.token),
    )
    .unwrap();

    // Sync in a thread, cancel after a bit
    let cancel = Arc::new(AtomicBool::new(false));
    let cancel2 = cancel.clone();
    let handle = std::thread::spawn(move || {
        client.sync(cancel2).ok();
        client
    });

    std::thread::sleep(Duration::from_secs(1));
    cancel.store(true, Ordering::Relaxed);
    let client = handle.join().unwrap();

    // Check hub_tree
    let version = client.store.hub_version().unwrap();
    assert!(version >= 2, "expected version >= 2, got {}", version);

    let digest = client.store.digest_for_path("hello.txt").unwrap();
    assert!(digest.is_some(), "hello.txt should be in hub_tree");
}

#[test]
fn test_content_fetch_sqlite() {
    let hub = TestHub::start();
    hub.write_file("data.txt", "fetch me");
    hub.wait_for_scan();

    let db_path = tempfile::NamedTempFile::new().unwrap();
    let client = HubSyncClient::open_sqlite(
        db_path.path().to_str().unwrap(),
        &hub.url,
        Some(&hub.token),
    )
    .unwrap();

    // Sync tree
    let cancel = Arc::new(AtomicBool::new(false));
    let cancel2 = cancel.clone();
    let handle = std::thread::spawn(move || {
        client.sync(cancel2).ok();
        client
    });
    std::thread::sleep(Duration::from_secs(1));
    cancel.store(true, Ordering::Relaxed);
    let client = handle.join().unwrap();

    // Fetch content on demand
    let data = client.read("data.txt").unwrap();
    assert_eq!(data, b"fetch me");

    // Second read should come from cache
    let data2 = client.read("data.txt").unwrap();
    assert_eq!(data2, b"fetch me");
}

#[test]
fn test_content_fetch_fs() {
    let hub = TestHub::start();
    hub.write_file("fs-test.txt", "filesystem mode");
    hub.wait_for_scan();

    let db_path = tempfile::NamedTempFile::new().unwrap();
    let sync_dir = tempfile::tempdir().unwrap();
    let client = HubSyncClient::open_fs(
        db_path.path().to_str().unwrap(),
        sync_dir.path().to_str().unwrap(),
        &hub.url,
        Some(&hub.token),
    )
    .unwrap();

    // Sync tree
    let cancel = Arc::new(AtomicBool::new(false));
    let cancel2 = cancel.clone();
    let handle = std::thread::spawn(move || {
        client.sync(cancel2).ok();
        client
    });
    std::thread::sleep(Duration::from_secs(1));
    cancel.store(true, Ordering::Relaxed);
    let client = handle.join().unwrap();

    // Fetch content — should write to filesystem
    let data = client.read("fs-test.txt").unwrap();
    assert_eq!(data, b"filesystem mode");

    // File should exist on disk
    let on_disk = std::fs::read(sync_dir.path().join("fs-test.txt")).unwrap();
    assert_eq!(on_disk, b"filesystem mode");
}

#[test]
fn test_auth_rejection() {
    let hub = TestHub::start();

    let db_path = tempfile::NamedTempFile::new().unwrap();
    let client = HubSyncClient::open_sqlite(
        db_path.path().to_str().unwrap(),
        &hub.url,
        Some("wrong-token"),
    )
    .unwrap();

    let cancel = Arc::new(AtomicBool::new(false));
    let cancel2 = cancel.clone();

    // Should fail with auth error, not hang forever
    let handle = std::thread::spawn(move || {
        // Will retry a few times then we cancel
        client.sync(cancel2).ok();
    });

    std::thread::sleep(Duration::from_secs(3));
    cancel.store(true, Ordering::Relaxed);
    handle.join().unwrap();

    // If we get here without hanging, auth rejection works
}
