use std::fs;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use rusqlite::{Connection, params};

use crate::error::Result;

/// ContentStore abstracts where blob data is stored.
pub trait ContentStore {
    /// Read content for a file. Returns None if not cached.
    fn read(&self, path: &str, digest: &str) -> Result<Option<Vec<u8>>>;

    /// Write content for a file.
    fn write(&self, path: &str, digest: &str, data: &[u8], mode: u32, mtime: i64) -> Result<()>;

    /// Check if content is cached.
    fn is_cached(&self, digest: &str) -> Result<bool>;

    /// Evict content for a specific digest.
    fn evict_digest(&self, digest: &str) -> Result<()>;

    /// Update accessed_at timestamp.
    fn touch(&self, digest: &str) -> Result<()>;

    /// Evict LRU content until total cache size is at or below target_bytes.
    /// Returns bytes freed. Respects pinned digests.
    fn evict_to(&self, target_bytes: u64) -> Result<u64>;

    /// Total cached bytes.
    fn cached_bytes(&self) -> Result<u64>;

    /// Initialize the content_cache table schema.
    fn init_schema(&self) -> Result<()>;
}

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64
}

// --- SQLite Backend ---

/// Stores blob data in content_cache.data (for iOS).
pub struct SqliteContentStore<'a> {
    db: &'a Connection,
}

impl<'a> SqliteContentStore<'a> {
    pub fn new(db: &'a Connection) -> Self {
        Self { db }
    }
}

impl ContentStore for SqliteContentStore<'_> {
    fn init_schema(&self) -> Result<()> {
        self.db.execute_batch(
            "CREATE TABLE IF NOT EXISTS content_cache (
                digest      TEXT PRIMARY KEY,
                data        BLOB NOT NULL,
                size        INTEGER NOT NULL,
                fetched_at  INTEGER NOT NULL,
                accessed_at INTEGER NOT NULL
            );",
        )?;
        Ok(())
    }

    fn read(&self, _path: &str, digest: &str) -> Result<Option<Vec<u8>>> {
        let result: std::result::Result<Vec<u8>, _> = self.db.query_row(
            "SELECT data FROM content_cache WHERE digest = ?1",
            params![digest],
            |row| row.get(0),
        );
        match result {
            Ok(data) => Ok(Some(data)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    fn write(&self, _path: &str, digest: &str, data: &[u8], _mode: u32, _mtime: i64) -> Result<()> {
        let now = now_secs();
        self.db.execute(
            "INSERT OR REPLACE INTO content_cache (digest, data, size, fetched_at, accessed_at)
             VALUES (?1, ?2, ?3, ?4, ?5)",
            params![digest, data, data.len() as i64, now, now],
        )?;
        Ok(())
    }

    fn is_cached(&self, digest: &str) -> Result<bool> {
        let count: i64 = self.db.query_row(
            "SELECT COUNT(*) FROM content_cache WHERE digest = ?1",
            params![digest],
            |row| row.get(0),
        )?;
        Ok(count > 0)
    }

    fn evict_digest(&self, digest: &str) -> Result<()> {
        self.db
            .execute("DELETE FROM content_cache WHERE digest = ?1", params![digest])?;
        Ok(())
    }

    fn touch(&self, digest: &str) -> Result<()> {
        let now = now_secs();
        self.db.execute(
            "UPDATE content_cache SET accessed_at = ?1 WHERE digest = ?2",
            params![now, digest],
        )?;
        Ok(())
    }

    fn evict_to(&self, target_bytes: u64) -> Result<u64> {
        let current = self.cached_bytes()?;
        if current <= target_bytes {
            return Ok(0);
        }
        let to_free = current - target_bytes;
        let mut freed: u64 = 0;

        let mut stmt = self.db.prepare(
            "SELECT digest, size FROM content_cache
             WHERE digest NOT IN (SELECT digest FROM pinned)
             ORDER BY accessed_at ASC",
        )?;
        let rows: Vec<(String, i64)> = stmt
            .query_map([], |row| Ok((row.get(0)?, row.get(1)?)))?
            .filter_map(|r| r.ok())
            .collect();

        for (digest, size) in rows {
            self.db
                .execute("DELETE FROM content_cache WHERE digest = ?1", params![digest])?;
            freed += size as u64;
            if freed >= to_free {
                break;
            }
        }
        Ok(freed)
    }

    fn cached_bytes(&self) -> Result<u64> {
        let bytes: i64 = self.db.query_row(
            "SELECT COALESCE(SUM(size), 0) FROM content_cache",
            [],
            |row| row.get(0),
        )?;
        Ok(bytes as u64)
    }
}

/// SqliteContentStore that owns its own DB connection (for use in HubSyncClient).
pub struct SqliteContentStoreOwned {
    db: Connection,
}

impl SqliteContentStoreOwned {
    pub fn new(db_path: &str) -> Result<Self> {
        let db = Connection::open(db_path)?;
        Ok(Self { db })
    }
}

impl ContentStore for SqliteContentStoreOwned {
    fn init_schema(&self) -> Result<()> {
        SqliteContentStore::new(&self.db).init_schema()
    }
    fn read(&self, path: &str, digest: &str) -> Result<Option<Vec<u8>>> {
        SqliteContentStore::new(&self.db).read(path, digest)
    }
    fn write(&self, path: &str, digest: &str, data: &[u8], mode: u32, mtime: i64) -> Result<()> {
        SqliteContentStore::new(&self.db).write(path, digest, data, mode, mtime)
    }
    fn is_cached(&self, digest: &str) -> Result<bool> {
        SqliteContentStore::new(&self.db).is_cached(digest)
    }
    fn evict_digest(&self, digest: &str) -> Result<()> {
        SqliteContentStore::new(&self.db).evict_digest(digest)
    }
    fn touch(&self, digest: &str) -> Result<()> {
        SqliteContentStore::new(&self.db).touch(digest)
    }
    fn evict_to(&self, target_bytes: u64) -> Result<u64> {
        SqliteContentStore::new(&self.db).evict_to(target_bytes)
    }
    fn cached_bytes(&self) -> Result<u64> {
        SqliteContentStore::new(&self.db).cached_bytes()
    }
}

// --- Filesystem Backend ---

/// Stores blob data as files on disk. content_cache tracks metadata only (no data column).
pub struct FsContentStore<'a> {
    db: &'a Connection,
    sync_dir: PathBuf,
}

impl<'a> FsContentStore<'a> {
    pub fn new(db: &'a Connection, sync_dir: &str) -> Self {
        Self {
            db,
            sync_dir: PathBuf::from(sync_dir),
        }
    }

    fn file_path(&self, path: &str) -> PathBuf {
        self.sync_dir.join(path)
    }
}

impl ContentStore for FsContentStore<'_> {
    fn init_schema(&self) -> Result<()> {
        self.db.execute_batch(
            "CREATE TABLE IF NOT EXISTS content_cache (
                digest      TEXT PRIMARY KEY,
                size        INTEGER NOT NULL,
                fetched_at  INTEGER NOT NULL,
                accessed_at INTEGER NOT NULL
            );",
        )?;
        Ok(())
    }

    fn read(&self, path: &str, _digest: &str) -> Result<Option<Vec<u8>>> {
        let full = self.file_path(path);
        if full.exists() {
            Ok(Some(fs::read(&full)?))
        } else {
            Ok(None)
        }
    }

    fn write(&self, path: &str, digest: &str, data: &[u8], mode: u32, mtime: i64) -> Result<()> {
        let full = self.file_path(path);
        if let Some(parent) = full.parent() {
            fs::create_dir_all(parent)?;
        }
        fs::write(&full, data)?;

        // Set permissions on unix
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            if mode != 0 {
                let _ = fs::set_permissions(&full, fs::Permissions::from_mode(mode));
            }
        }

        // Set mtime
        let _ = set_mtime(&full, mtime);

        let now = now_secs();
        self.db.execute(
            "INSERT OR REPLACE INTO content_cache (digest, size, fetched_at, accessed_at)
             VALUES (?1, ?2, ?3, ?4)",
            params![digest, data.len() as i64, now, now],
        )?;
        Ok(())
    }

    fn is_cached(&self, digest: &str) -> Result<bool> {
        let count: i64 = self.db.query_row(
            "SELECT COUNT(*) FROM content_cache WHERE digest = ?1",
            params![digest],
            |row| row.get(0),
        )?;
        Ok(count > 0)
    }

    fn evict_digest(&self, digest: &str) -> Result<()> {
        // Find all paths with this digest and delete the files
        let mut stmt = self.db.prepare(
            // Join is on the outer store's hub_tree, but we reference it here
            // since both tables are in the same DB.
            "SELECT path FROM hub_tree WHERE digest = ?1",
        )?;
        let paths: Vec<String> = stmt
            .query_map(params![digest], |row| row.get(0))?
            .filter_map(|r| r.ok())
            .collect();

        for path in paths {
            let _ = fs::remove_file(self.file_path(&path));
        }

        self.db
            .execute("DELETE FROM content_cache WHERE digest = ?1", params![digest])?;
        Ok(())
    }

    fn touch(&self, digest: &str) -> Result<()> {
        let now = now_secs();
        self.db.execute(
            "UPDATE content_cache SET accessed_at = ?1 WHERE digest = ?2",
            params![now, digest],
        )?;
        Ok(())
    }

    fn evict_to(&self, target_bytes: u64) -> Result<u64> {
        let current = self.cached_bytes()?;
        if current <= target_bytes {
            return Ok(0);
        }
        let to_free = current - target_bytes;
        let mut freed: u64 = 0;

        let mut stmt = self.db.prepare(
            "SELECT digest, size FROM content_cache
             WHERE digest NOT IN (SELECT digest FROM pinned)
             ORDER BY accessed_at ASC",
        )?;
        let rows: Vec<(String, i64)> = stmt
            .query_map([], |row| Ok((row.get(0)?, row.get(1)?)))?
            .filter_map(|r| r.ok())
            .collect();

        for (digest, size) in rows {
            self.evict_digest(&digest)?;
            freed += size as u64;
            if freed >= to_free {
                break;
            }
        }
        Ok(freed)
    }

    fn cached_bytes(&self) -> Result<u64> {
        let bytes: i64 = self.db.query_row(
            "SELECT COALESCE(SUM(size), 0) FROM content_cache",
            [],
            |row| row.get(0),
        )?;
        Ok(bytes as u64)
    }
}

/// FsContentStore that owns its own DB connection (for use in HubSyncClient).
pub struct FsContentStoreOwned {
    db: Connection,
    sync_dir: PathBuf,
}

impl FsContentStoreOwned {
    pub fn new(db_path: &str, sync_dir: &str) -> Result<Self> {
        let db = Connection::open(db_path)?;
        Ok(Self {
            db,
            sync_dir: PathBuf::from(sync_dir),
        })
    }
}

impl ContentStore for FsContentStoreOwned {
    fn init_schema(&self) -> Result<()> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).init_schema()
    }
    fn read(&self, path: &str, digest: &str) -> Result<Option<Vec<u8>>> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).read(path, digest)
    }
    fn write(&self, path: &str, digest: &str, data: &[u8], mode: u32, mtime: i64) -> Result<()> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).write(path, digest, data, mode, mtime)
    }
    fn is_cached(&self, digest: &str) -> Result<bool> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).is_cached(digest)
    }
    fn evict_digest(&self, digest: &str) -> Result<()> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).evict_digest(digest)
    }
    fn touch(&self, digest: &str) -> Result<()> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).touch(digest)
    }
    fn evict_to(&self, target_bytes: u64) -> Result<u64> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).evict_to(target_bytes)
    }
    fn cached_bytes(&self) -> Result<u64> {
        FsContentStore::new(&self.db, self.sync_dir.to_str().unwrap()).cached_bytes()
    }
}

#[allow(unused_variables)]
fn set_mtime(path: &Path, mtime: i64) -> std::io::Result<()> {
    // filetime crate would be ideal, but keeping deps minimal.
    // On unix, use libc::utimensat.
    #[cfg(unix)]
    {
        use std::ffi::CString;
        use std::os::unix::ffi::OsStrExt;

        let c_path = CString::new(path.as_os_str().as_bytes())?;
        let times = [
            libc::timespec {
                tv_sec: mtime,
                tv_nsec: 0,
            },
            libc::timespec {
                tv_sec: mtime,
                tv_nsec: 0,
            },
        ];
        unsafe {
            libc::utimensat(libc::AT_FDCWD, c_path.as_ptr(), times.as_ptr(), 0);
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_db() -> Connection {
        let db = Connection::open_in_memory().unwrap();
        db.execute_batch(
            "CREATE TABLE IF NOT EXISTS hub_tree (
                path TEXT PRIMARY KEY, kind INTEGER NOT NULL DEFAULT 0,
                digest TEXT, size INTEGER, mode INTEGER, mtime INTEGER, version INTEGER NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_hub_tree_digest ON hub_tree(digest);
            CREATE TABLE IF NOT EXISTS pinned (digest TEXT PRIMARY KEY, pinned_at INTEGER NOT NULL);",
        )
        .unwrap();
        db
    }

    #[test]
    fn test_sqlite_store_write_read() {
        let db = test_db();
        let store = SqliteContentStore::new(&db);
        store.init_schema().unwrap();

        store
            .write("file.txt", "abc123", b"hello", 0o644, 1000)
            .unwrap();

        let data = store.read("file.txt", "abc123").unwrap().unwrap();
        assert_eq!(data, b"hello");
    }

    #[test]
    fn test_sqlite_store_not_cached() {
        let db = test_db();
        let store = SqliteContentStore::new(&db);
        store.init_schema().unwrap();

        assert!(!store.is_cached("nonexistent").unwrap());
        assert!(store.read("file.txt", "nonexistent").unwrap().is_none());
    }

    #[test]
    fn test_sqlite_store_evict() {
        let db = test_db();
        let store = SqliteContentStore::new(&db);
        store.init_schema().unwrap();

        store
            .write("a.txt", "d1", b"aaaa", 0o644, 1000)
            .unwrap();
        store
            .write("b.txt", "d2", b"bbbb", 0o644, 1000)
            .unwrap();

        assert_eq!(store.cached_bytes().unwrap(), 8);

        store.evict_digest("d1").unwrap();
        assert_eq!(store.cached_bytes().unwrap(), 4);
        assert!(!store.is_cached("d1").unwrap());
    }

    #[test]
    fn test_sqlite_store_evict_to() {
        let db = test_db();
        let store = SqliteContentStore::new(&db);
        store.init_schema().unwrap();

        store
            .write("a.txt", "d1", &[0u8; 100], 0o644, 1000)
            .unwrap();
        store
            .write("b.txt", "d2", &[0u8; 100], 0o644, 1000)
            .unwrap();
        store
            .write("c.txt", "d3", &[0u8; 100], 0o644, 1000)
            .unwrap();

        let freed = store.evict_to(150).unwrap();
        assert!(freed >= 100);
        assert!(store.cached_bytes().unwrap() <= 200);
    }

    #[test]
    fn test_sqlite_store_evict_respects_pins() {
        let db = test_db();
        let store = SqliteContentStore::new(&db);
        store.init_schema().unwrap();

        store
            .write("a.txt", "d1", &[0u8; 100], 0o644, 1000)
            .unwrap();
        store
            .write("b.txt", "d2", &[0u8; 100], 0o644, 1000)
            .unwrap();

        // Pin d1
        db.execute(
            "INSERT INTO pinned (digest, pinned_at) VALUES ('d1', 1000)",
            [],
        )
        .unwrap();

        let freed = store.evict_to(0).unwrap();
        // d1 should remain (pinned), only d2 evicted
        assert_eq!(freed, 100);
        assert!(store.is_cached("d1").unwrap());
        assert!(!store.is_cached("d2").unwrap());
    }

    #[test]
    fn test_fs_store_write_read() {
        let dir = tempfile::tempdir().unwrap();
        let db = test_db();
        let store = FsContentStore::new(&db, dir.path().to_str().unwrap());
        store.init_schema().unwrap();

        store
            .write("sub/file.txt", "abc123", b"hello world", 0o644, 1000)
            .unwrap();

        // File should exist on disk
        let on_disk = std::fs::read(dir.path().join("sub/file.txt")).unwrap();
        assert_eq!(on_disk, b"hello world");

        // Read through store
        let data = store.read("sub/file.txt", "abc123").unwrap().unwrap();
        assert_eq!(data, b"hello world");

        assert!(store.is_cached("abc123").unwrap());
    }

    #[test]
    fn test_fs_store_evict() {
        let dir = tempfile::tempdir().unwrap();
        let db = test_db();
        // Insert a hub_tree row so evict can find the path
        db.execute(
            "INSERT INTO hub_tree (path, kind, digest, size, mode, mtime, version)
             VALUES ('file.txt', 0, 'abc', 5, 420, 1000, 1)",
            [],
        )
        .unwrap();

        let store = FsContentStore::new(&db, dir.path().to_str().unwrap());
        store.init_schema().unwrap();

        store
            .write("file.txt", "abc", b"hello", 0o644, 1000)
            .unwrap();
        assert!(dir.path().join("file.txt").exists());

        store.evict_digest("abc").unwrap();
        assert!(!dir.path().join("file.txt").exists());
        assert!(!store.is_cached("abc").unwrap());
    }
}
