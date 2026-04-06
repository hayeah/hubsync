use std::io::{Read as _, Write as _};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use reqwest::blocking::Client;
use reqwest::header::{AUTHORIZATION, HeaderValue};

use crate::content::ContentStore;
use crate::error::{Error, Result};
use crate::proto::sync_event;
use crate::protocol::{read_length_prefixed, sha256_hex};
use crate::store::Store;

/// HubSyncClient syncs tree metadata from a hub and provides on-demand content fetching.
pub struct HubSyncClient {
    pub store: Store,
    content: Box<dyn ContentStore + Send>,
    hub_url: String,
    token: Option<String>,
    http: Client,         // for blob fetches (no timeout)
    http_stream: Client,  // for subscribe (short read timeout for cancel checks)
}

impl HubSyncClient {
    /// Create a client with a SQLite content backend.
    pub fn open_sqlite(db_path: &str, hub_url: &str, token: Option<&str>) -> Result<Self> {
        let store = Store::open(db_path)?;
        let content = Box::new(crate::content::SqliteContentStoreOwned::new(db_path)?);
        Self::new(store, content, hub_url, token)
    }

    /// Create a client with a filesystem content backend.
    pub fn open_fs(
        db_path: &str,
        sync_dir: &str,
        hub_url: &str,
        token: Option<&str>,
    ) -> Result<Self> {
        let store = Store::open(db_path)?;
        let content = Box::new(crate::content::FsContentStoreOwned::new(db_path, sync_dir)?);
        Self::new(store, content, hub_url, token)
    }

    fn new(
        store: Store,
        content: Box<dyn ContentStore + Send>,
        hub_url: &str,
        token: Option<&str>,
    ) -> Result<Self> {
        content.init_schema()?;
        Ok(Self {
            store,
            content,
            hub_url: hub_url.trim_end_matches('/').to_string(),
            token: token.map(|s| s.to_string()),
            http: Client::builder().build()?,
            http_stream: Client::builder()
                .timeout(Some(Duration::from_secs(2)))
                .build()?,
        })
    }

    /// Sync tree metadata from the hub. Blocks until cancelled or connection drops.
    /// Reconnects automatically on failure.
    pub fn sync(&self, cancel: Arc<AtomicBool>) -> Result<()> {
        self.sync_with_callback(cancel, |_| {})
    }

    /// Sync with a callback fired after each event is applied.
    pub fn sync_with_callback(
        &self,
        cancel: Arc<AtomicBool>,
        mut on_event: impl FnMut(&crate::proto::SyncEvent),
    ) -> Result<()> {
        loop {
            if cancel.load(Ordering::Relaxed) {
                return Ok(());
            }
            match self.sync_once_cb(&cancel, &mut on_event) {
                Ok(()) => return Ok(()),
                Err(e) => {
                    if cancel.load(Ordering::Relaxed) {
                        return Ok(());
                    }
                    eprintln!("sync error: {}, reconnecting in 1s", e);
                    std::thread::sleep(Duration::from_secs(1));
                }
            }
        }
    }

    fn sync_once(&self, cancel: &Arc<AtomicBool>) -> Result<()> {
        self.sync_once_cb(cancel, &mut |_| {})
    }

    /// Bootstrap by fetching the tree DB from /snapshots-tree/latest.
    /// Replaces the local hub_tree and sync_state with the hub's current state.
    /// Only needed on first sync (hub_version == 0).
    fn bootstrap(&self) -> Result<()> {
        let url = format!("{}/snapshots-tree/latest", self.hub_url);
        let mut req = self.http.get(&url);
        if let Some(ref token) = self.token {
            req = req.header(AUTHORIZATION, format!("Bearer {}", token));
        }

        let resp = req.send()?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().unwrap_or_default();
            return Err(Error::Other(format!("bootstrap tree db {}: {}", status, body)));
        }

        // Write to a temp file, then import into our store
        let mut data = Vec::new();
        resp.bytes()?.as_ref().read_to_end(&mut data)?;

        let tmp_path = format!("{}.bootstrap.tmp", self.store.db_path());
        {
            let mut f = std::fs::File::create(&tmp_path)?;
            f.write_all(&data)?;
        }

        // Import the tree from the downloaded DB
        self.store.import_tree_db(&tmp_path)?;
        let _ = std::fs::remove_file(&tmp_path);

        let version = self.store.hub_version()?;
        eprintln!("bootstrap complete, version={}", version);
        Ok(())
    }

    fn sync_once_cb(
        &self,
        cancel: &Arc<AtomicBool>,
        on_event: &mut impl FnMut(&crate::proto::SyncEvent),
    ) -> Result<()> {
        let version = self.store.hub_version()?;

        // Bootstrap on first sync instead of replaying from version 0
        if version == 0 {
            self.bootstrap()?;
        }

        let version = self.store.hub_version()?;
        let url = format!("{}/sync/subscribe?since={}", self.hub_url, version);

        let mut req = self.http_stream.get(&url);
        if let Some(ref token) = self.token {
            req = req.header(AUTHORIZATION, format!("Bearer {}", token));
        }

        let resp = req.send()?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().unwrap_or_default();
            return Err(Error::Other(format!("subscribe {}: {}", status, body)));
        }

        let mut reader = resp;
        loop {
            if cancel.load(Ordering::Relaxed) {
                return Ok(());
            }

            let event = match read_length_prefixed(&mut reader) {
                Ok(e) => e,
                Err(Error::Io(e))
                    if e.kind() == std::io::ErrorKind::UnexpectedEof
                        || e.kind() == std::io::ErrorKind::TimedOut
                        || e.kind() == std::io::ErrorKind::WouldBlock =>
                {
                    // Read timeout or connection closed — check cancel and retry
                    if cancel.load(Ordering::Relaxed) {
                        return Ok(());
                    }
                    continue;
                }
                Err(e) => return Err(e),
            };

            self.apply_event(&event)?;
            on_event(&event);
        }
    }

    fn apply_event(&self, event: &crate::proto::SyncEvent) -> Result<()> {
        let version = event.version as i64;
        let path = &event.path;

        match &event.event {
            Some(sync_event::Event::Change(change)) => {
                let digest = hex::encode(&change.digest);
                self.store.apply_change(
                    version,
                    path,
                    change.kind,
                    &digest,
                    change.size as i64,
                    change.mode,
                    change.mtime,
                )?;
            }
            Some(sync_event::Event::Delete(_)) => {
                // Evict content if using fs backend
                if let Some(digest) = self.store.digest_for_path(path)? {
                    // Don't evict — other paths may reference the same digest.
                    // Let LRU handle it.
                    let _ = digest;
                }
                self.store.apply_delete(version, path)?;
            }
            None => {}
        }
        Ok(())
    }

    /// Read file content. Fetches from hub on demand if not cached.
    pub fn read(&self, path: &str) -> Result<Vec<u8>> {
        let digest = self
            .store
            .digest_for_path(path)?
            .ok_or_else(|| Error::NotFound(path.to_string()))?;

        // Check cache
        if let Some(data) = self.content.read(path, &digest)? {
            self.content.touch(&digest)?;
            return Ok(data);
        }

        // Fetch from hub
        let data = self.fetch_blob(&digest)?;

        // Verify digest
        let got = sha256_hex(&data);
        if got != digest {
            return Err(Error::DigestMismatch {
                got,
                want: digest.clone(),
            });
        }

        // Get metadata for write
        let (mode, mtime) = self.file_metadata(path);
        self.content.write(path, &digest, &data, mode, mtime)?;
        Ok(data)
    }

    /// Prefetch content for files matching a glob pattern.
    /// Returns total bytes fetched.
    pub fn prefetch(&self, glob_pattern: &str) -> Result<u64> {
        let mut stmt = self.store.conn().prepare(
            "SELECT path, digest, size FROM hub_tree
             WHERE kind = 0 AND digest IS NOT NULL",
        )?;
        let rows: Vec<(String, String, i64)> = stmt
            .query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))?
            .filter_map(|r| r.ok())
            .collect();

        let mut fetched: u64 = 0;
        for (path, digest, _size) in rows {
            if !glob_match::glob_match(glob_pattern, &path) {
                continue;
            }
            if self.content.is_cached(&digest)? {
                continue;
            }
            match self.fetch_blob(&digest) {
                Ok(data) => {
                    let (mode, mtime) = self.file_metadata(&path);
                    self.content.write(&path, &digest, &data, mode, mtime)?;
                    fetched += data.len() as u64;
                }
                Err(e) => {
                    eprintln!("prefetch {}: {}", path, e);
                }
            }
        }
        Ok(fetched)
    }

    /// Evict cached content to stay within target_bytes. Returns bytes freed.
    pub fn evict(&self, target_bytes: u64) -> Result<u64> {
        self.content.evict_to(target_bytes)
    }

    /// Set the cache budget in bytes.
    pub fn set_cache_budget(&self, bytes: u64) -> Result<()> {
        self.store.conn().execute(
            "INSERT OR REPLACE INTO sync_state (key, value) VALUES ('cache_budget', ?1)",
            rusqlite::params![bytes.to_string()],
        )?;
        Ok(())
    }

    /// Pin files matching a glob pattern.
    pub fn pin(&self, glob: &str) -> Result<u64> {
        self.store.pin(glob)
    }

    /// Unpin files matching a glob pattern.
    pub fn unpin(&self, glob: &str) -> Result<u64> {
        self.store.unpin(glob)
    }

    fn fetch_blob(&self, digest: &str) -> Result<Vec<u8>> {
        let url = format!("{}/blobs/{}", self.hub_url, digest);
        let mut req = self.http.get(&url);
        if let Some(ref token) = self.token {
            req = req.header(AUTHORIZATION, HeaderValue::from_str(&format!("Bearer {}", token)).unwrap());
        }
        let resp = req.send()?;
        if !resp.status().is_success() {
            return Err(Error::Other(format!(
                "blob fetch {}: {}",
                digest,
                resp.status()
            )));
        }
        Ok(resp.bytes()?.to_vec())
    }

    fn file_metadata(&self, path: &str) -> (u32, i64) {
        // Query hub_tree for mode and mtime
        let result: std::result::Result<(u32, i64), _> = self.store.conn().query_row(
            "SELECT COALESCE(mode, 420), COALESCE(mtime, 0) FROM hub_tree WHERE path = ?1",
            rusqlite::params![path],
            |row| Ok((row.get(0)?, row.get(1)?)),
        );
        result.unwrap_or((0o644, 0))
    }
}
