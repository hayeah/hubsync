use rusqlite::{Connection, params};

use crate::error::Result;

/// Store manages the hub_tree, sync_state, and pinned tables.
/// Content storage is handled separately by ContentStore implementations.
pub struct Store {
    db: Connection,
    path: String,
}

impl Store {
    /// Open or create the sync database and initialize schema.
    pub fn open(path: &str) -> Result<Self> {
        let db = Connection::open(path)?;
        db.execute_batch("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;")?;
        let store = Store { db, path: path.to_string() };
        store.init_schema()?;
        Ok(store)
    }

    /// Get the database file path.
    pub fn db_path(&self) -> &str {
        &self.path
    }

    fn init_schema(&self) -> Result<()> {
        self.db.execute_batch(
            "CREATE TABLE IF NOT EXISTS hub_tree (
                path      TEXT PRIMARY KEY,
                kind      INTEGER NOT NULL DEFAULT 0,
                digest    TEXT,
                size      INTEGER,
                mode      INTEGER,
                mtime     INTEGER,
                version   INTEGER NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_hub_tree_digest ON hub_tree(digest);

            CREATE TABLE IF NOT EXISTS sync_state (
                key   TEXT PRIMARY KEY,
                value TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS pinned (
                digest    TEXT PRIMARY KEY,
                pinned_at INTEGER NOT NULL
            );",
        )?;
        Ok(())
    }

    /// Get the underlying database connection (for direct queries).
    pub fn conn(&self) -> &Connection {
        &self.db
    }

    /// Get the last synced hub version, or 0 if never synced.
    pub fn hub_version(&self) -> Result<i64> {
        let result: std::result::Result<String, _> = self.db.query_row(
            "SELECT value FROM sync_state WHERE key = 'hub_version'",
            [],
            |row| row.get(0),
        );
        match result {
            Ok(v) => Ok(v.parse::<i64>().unwrap_or(0)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(0),
            Err(e) => Err(e.into()),
        }
    }

    /// Set the hub version.
    pub fn set_hub_version(&self, version: i64) -> Result<()> {
        self.db.execute(
            "INSERT OR REPLACE INTO sync_state (key, value) VALUES ('hub_version', ?1)",
            params![version.to_string()],
        )?;
        Ok(())
    }

    /// Apply a file change event (create or update) to hub_tree.
    pub fn apply_change(
        &self,
        version: i64,
        path: &str,
        kind: i32,
        digest: &str,
        size: i64,
        mode: u32,
        mtime: i64,
    ) -> Result<()> {
        let tx = self.db.unchecked_transaction()?;
        tx.execute(
            "INSERT OR REPLACE INTO hub_tree (path, kind, digest, size, mode, mtime, version)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            params![path, kind, digest, size, mode, mtime, version],
        )?;
        tx.execute(
            "INSERT OR REPLACE INTO sync_state (key, value) VALUES ('hub_version', ?1)",
            params![version.to_string()],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// Apply a file delete event to hub_tree.
    pub fn apply_delete(&self, version: i64, path: &str) -> Result<()> {
        let tx = self.db.unchecked_transaction()?;
        tx.execute("DELETE FROM hub_tree WHERE path = ?1", params![path])?;
        tx.execute(
            "INSERT OR REPLACE INTO sync_state (key, value) VALUES ('hub_version', ?1)",
            params![version.to_string()],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// Look up a file's digest by path.
    pub fn digest_for_path(&self, path: &str) -> Result<Option<String>> {
        let result: std::result::Result<String, _> = self.db.query_row(
            "SELECT digest FROM hub_tree WHERE path = ?1",
            params![path],
            |row| row.get(0),
        );
        match result {
            Ok(d) => Ok(Some(d)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    /// Find a path with the given digest (for dedup), excluding a specific path.
    pub fn path_by_digest(&self, digest: &str, exclude_path: &str) -> Result<Option<String>> {
        let result: std::result::Result<String, _> = self.db.query_row(
            "SELECT path FROM hub_tree WHERE digest = ?1 AND path != ?2 LIMIT 1",
            params![digest, exclude_path],
            |row| row.get(0),
        );
        match result {
            Ok(p) => Ok(Some(p)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    /// Pin all files matching a glob pattern.
    pub fn pin(&self, glob: &str) -> Result<u64> {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;
        let count = self.db.execute(
            "INSERT OR IGNORE INTO pinned (digest, pinned_at)
             SELECT digest, ?1 FROM hub_tree WHERE path GLOB ?2 AND digest IS NOT NULL",
            params![now, glob],
        )?;
        Ok(count as u64)
    }

    /// Count entries in hub_tree.
    pub fn entry_count(&self) -> Result<u64> {
        let count: i64 = self.db.query_row(
            "SELECT COUNT(*) FROM hub_tree",
            [],
            |row| row.get(0),
        )?;
        Ok(count as u64)
    }

    /// Import hub_tree and sync_state from a downloaded snapshot DB.
    /// Replaces all existing hub_tree entries and updates hub_version.
    pub fn import_tree_db(&self, snapshot_path: &str) -> Result<()> {
        self.db.execute(
            "ATTACH DATABASE ?1 AS snapshot",
            params![snapshot_path],
        )?;
        let result = (|| -> Result<()> {
            let tx = self.db.unchecked_transaction()?;
            tx.execute_batch(
                "DELETE FROM hub_tree;
                 INSERT INTO hub_tree SELECT * FROM snapshot.hub_tree;
                 INSERT OR REPLACE INTO sync_state
                     SELECT key, value FROM snapshot.sync_state;",
            )?;
            tx.commit()?;
            Ok(())
        })();
        self.db.execute_batch("DETACH DATABASE snapshot")?;
        result
    }

    /// Unpin all files matching a glob pattern.
    pub fn unpin(&self, glob: &str) -> Result<u64> {
        let count = self.db.execute(
            "DELETE FROM pinned WHERE digest IN
             (SELECT digest FROM hub_tree WHERE path GLOB ?1)",
            params![glob],
        )?;
        Ok(count as u64)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_store() -> Store {
        Store::open(":memory:").unwrap()
    }

    #[test]
    fn test_hub_version_default() {
        let store = test_store();
        assert_eq!(store.hub_version().unwrap(), 0);
    }

    #[test]
    fn test_hub_version_set() {
        let store = test_store();
        store.set_hub_version(42).unwrap();
        assert_eq!(store.hub_version().unwrap(), 42);
    }

    #[test]
    fn test_apply_change() {
        let store = test_store();
        store
            .apply_change(1, "README.md", 0, "aabbcc", 100, 0o644, 1000)
            .unwrap();
        assert_eq!(store.hub_version().unwrap(), 1);
        assert_eq!(
            store.digest_for_path("README.md").unwrap(),
            Some("aabbcc".into())
        );
    }

    #[test]
    fn test_apply_delete() {
        let store = test_store();
        store
            .apply_change(1, "file.txt", 0, "abc", 10, 0o644, 1000)
            .unwrap();
        store.apply_delete(2, "file.txt").unwrap();
        assert_eq!(store.hub_version().unwrap(), 2);
        assert_eq!(store.digest_for_path("file.txt").unwrap(), None);
    }

    #[test]
    fn test_path_by_digest() {
        let store = test_store();
        store
            .apply_change(1, "a.txt", 0, "shared_digest", 10, 0o644, 1000)
            .unwrap();
        store
            .apply_change(2, "b.txt", 0, "shared_digest", 10, 0o644, 1000)
            .unwrap();

        let path = store
            .path_by_digest("shared_digest", "a.txt")
            .unwrap()
            .unwrap();
        assert_eq!(path, "b.txt");
    }

    #[test]
    fn test_pin_unpin() {
        let store = test_store();
        store
            .apply_change(1, "docs/a.txt", 0, "d1", 10, 0o644, 1000)
            .unwrap();
        store
            .apply_change(2, "docs/b.txt", 0, "d2", 10, 0o644, 1000)
            .unwrap();
        store
            .apply_change(3, "src/c.txt", 0, "d3", 10, 0o644, 1000)
            .unwrap();

        let pinned = store.pin("docs/*").unwrap();
        assert_eq!(pinned, 2);

        let unpinned = store.unpin("docs/*").unwrap();
        assert_eq!(unpinned, 2);
    }
}
