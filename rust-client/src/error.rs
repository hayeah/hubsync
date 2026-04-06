use thiserror::Error;

#[derive(Error, Debug)]
pub enum Error {
    #[error("sqlite error: {0}")]
    Sqlite(#[from] rusqlite::Error),

    #[error("http error: {0}")]
    Http(#[from] reqwest::Error),

    #[error("io error: {0}")]
    Io(#[from] std::io::Error),

    #[error("protobuf decode error: {0}")]
    Decode(#[from] prost::DecodeError),

    #[error("not found: {0}")]
    NotFound(String),

    #[error("digest mismatch: got {got}, want {want}")]
    DigestMismatch { got: String, want: String },

    #[error("{0}")]
    Other(String),
}

pub type Result<T> = std::result::Result<T, Error>;
