use std::io::Read;

use prost::Message;

use crate::error::{Error, Result};
use crate::proto::SyncEvent;

/// Read one length-prefixed protobuf message from a reader.
/// Format: [4 bytes big-endian length][protobuf bytes]
pub fn read_length_prefixed(reader: &mut impl Read) -> Result<SyncEvent> {
    let mut len_buf = [0u8; 4];
    reader.read_exact(&mut len_buf).map_err(|e| {
        if e.kind() == std::io::ErrorKind::UnexpectedEof {
            Error::Io(e)
        } else {
            Error::Io(e)
        }
    })?;
    let len = u32::from_be_bytes(len_buf) as usize;

    if len > 64 * 1024 * 1024 {
        return Err(Error::Other(format!("message too large: {} bytes", len)));
    }

    let mut buf = vec![0u8; len];
    reader.read_exact(&mut buf)?;
    let event = SyncEvent::decode(&buf[..])?;
    Ok(event)
}

/// Compute SHA-256 digest of data, returned as hex string.
pub fn sha256_hex(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let hash = Sha256::digest(data);
    hex::encode(hash)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{FileChange, FileDelete, sync_event};

    fn encode_event(event: &SyncEvent) -> Vec<u8> {
        let data = event.encode_to_vec();
        let mut buf = Vec::new();
        buf.extend_from_slice(&(data.len() as u32).to_be_bytes());
        buf.extend_from_slice(&data);
        buf
    }

    #[test]
    fn test_read_change_event() {
        let event = SyncEvent {
            version: 1,
            path: "test.txt".into(),
            event: Some(sync_event::Event::Change(FileChange {
                kind: 0,
                digest: vec![0xaa, 0xbb],
                size: 100,
                mode: 0o644,
                mtime: 1000,
                data: b"hello".to_vec(),
            })),
        };

        let encoded = encode_event(&event);
        let mut cursor = std::io::Cursor::new(encoded);
        let decoded = read_length_prefixed(&mut cursor).unwrap();

        assert_eq!(decoded.version, 1);
        assert_eq!(decoded.path, "test.txt");
        match decoded.event.unwrap() {
            sync_event::Event::Change(c) => {
                assert_eq!(c.size, 100);
                assert_eq!(c.data, b"hello");
            }
            _ => panic!("expected Change event"),
        }
    }

    #[test]
    fn test_read_delete_event() {
        let event = SyncEvent {
            version: 5,
            path: "deleted.txt".into(),
            event: Some(sync_event::Event::Delete(FileDelete {})),
        };

        let encoded = encode_event(&event);
        let mut cursor = std::io::Cursor::new(encoded);
        let decoded = read_length_prefixed(&mut cursor).unwrap();

        assert_eq!(decoded.version, 5);
        assert!(matches!(
            decoded.event.unwrap(),
            sync_event::Event::Delete(_)
        ));
    }

    #[test]
    fn test_read_multiple_events() {
        let mut buf = Vec::new();
        for i in 1..=3 {
            let event = SyncEvent {
                version: i,
                path: format!("file{}.txt", i),
                event: Some(sync_event::Event::Change(FileChange {
                    kind: 0,
                    digest: vec![],
                    size: i * 10,
                    mode: 0o644,
                    mtime: 1000 + i as i64,
                    data: vec![],
                })),
            };
            buf.extend(encode_event(&event));
        }

        let mut cursor = std::io::Cursor::new(buf);
        for i in 1..=3u64 {
            let event = read_length_prefixed(&mut cursor).unwrap();
            assert_eq!(event.version, i);
        }
    }

    #[test]
    fn test_sha256_hex() {
        let hash = sha256_hex(b"hello");
        assert_eq!(
            hash,
            "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
        );
    }
}
