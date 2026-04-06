pub mod proto {
    include!(concat!(env!("OUT_DIR"), "/hubsync.rs"));
}

pub mod client;
pub mod content;
pub mod error;
pub mod ffi;
pub mod protocol;
pub mod store;

pub use client::HubSyncClient;
pub use content::{ContentStore, FsContentStore, SqliteContentStore};
pub use error::Error;
pub use store::Store;
