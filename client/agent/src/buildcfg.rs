//! Configuration embedded in a branded build by the client builder.
//!
//! Layout at the end of the executable, before the Authenticode signature (if any):
//! `[UTF-8 JSON][u32 LE length of the JSON]["RHISCFG1"]`.
use std::io::{Read, Seek, SeekFrom};
use std::path::Path;

use anyhow::{Result, bail};
use serde::{Deserialize, Serialize};

const MAGIC: &[u8; 8] = b"RHISCFG1";

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default)]
pub struct BuildConfig {
    pub v: u32,
    pub build_id: String,
    pub product_name: String,
    pub company_name: String,
    /// Base64 PNG.
    pub logo_png: String,
    pub accent_color: String,
    /// Server URLs in failover order.
    pub servers: Vec<String>,
    pub enroll_token: String,
    /// "install" or "quick".
    pub mode: String,
    /// "production" or "test".
    pub channel: String,
    /// Base64 ed25519 public keys for update signatures.
    pub update_keys: Vec<String>,
}

/// End of the data that belongs to the image: the start of the certificate table, or the file size.
fn pe_data_end<R: Read + Seek>(f: &mut R, size: u64) -> Result<u64> {
    let mut buf = [0u8; 4];
    f.seek(SeekFrom::Start(0x3c))?;
    f.read_exact(&mut buf)?;
    let pe = u32::from_le_bytes(buf) as u64;
    f.seek(SeekFrom::Start(pe))?;
    f.read_exact(&mut buf)?;
    if &buf != b"PE\0\0" {
        return Ok(size);
    }
    let opt = pe + 24;
    let mut magic = [0u8; 2];
    f.seek(SeekFrom::Start(opt))?;
    f.read_exact(&mut magic)?;
    let dirs = match u16::from_le_bytes(magic) {
        0x20b => opt + 112,
        0x10b => opt + 96,
        _ => return Ok(size),
    };
    // Data directory 4 is the certificate table; its "address" is a file offset.
    let mut ent = [0u8; 8];
    f.seek(SeekFrom::Start(dirs + 4 * 8))?;
    f.read_exact(&mut ent)?;
    let off = u32::from_le_bytes(ent[..4].try_into().unwrap()) as u64;
    Ok(if off > 0 && off <= size { off } else { size })
}

pub fn read_from<R: Read + Seek>(f: &mut R) -> Result<Option<BuildConfig>> {
    let size = f.seek(SeekFrom::End(0))?;
    let end = pe_data_end(f, size).unwrap_or(size);
    if end < 12 {
        return Ok(None);
    }
    let mut tail = [0u8; 12];
    f.seek(SeekFrom::Start(end - 12))?;
    f.read_exact(&mut tail)?;
    if &tail[4..] != MAGIC {
        return Ok(None);
    }
    let len = u32::from_le_bytes(tail[..4].try_into().unwrap()) as u64;
    if len == 0 || len > 4 << 20 || len + 12 > end {
        bail!("embedded configuration has a bad length");
    }
    let mut json = vec![0u8; len as usize];
    f.seek(SeekFrom::Start(end - 12 - len))?;
    f.read_exact(&mut json)?;
    Ok(Some(serde_json::from_slice(&json)?))
}

/// Reads the embedded configuration of an executable, if it has one.
pub fn read(path: &Path) -> Result<Option<BuildConfig>> {
    read_from(&mut std::fs::File::open(path)?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    fn with_trailer(mut body: Vec<u8>, json: &str) -> Vec<u8> {
        body.extend_from_slice(json.as_bytes());
        body.extend_from_slice(&(json.len() as u32).to_le_bytes());
        body.extend_from_slice(MAGIC);
        body
    }

    #[test]
    fn reads_trailer() {
        let f = with_trailer(
            vec![0u8; 100],
            r#"{"v":1,"servers":["https://a.example"],"enroll_token":"t"}"#,
        );
        let c = read_from(&mut Cursor::new(f)).unwrap().unwrap();
        assert_eq!(c.servers, vec!["https://a.example"]);
        assert_eq!(c.enroll_token, "t");
        assert_eq!(read_from(&mut Cursor::new(vec![0u8; 100])).unwrap(), None);
    }

    #[test]
    fn reads_trailer_before_signature() {
        // A minimal PE32+ header whose certificate table starts after the trailer.
        let mut f = vec![0u8; 0x200];
        f[0x3c..0x40].copy_from_slice(&0x80u32.to_le_bytes());
        f[0x80..0x84].copy_from_slice(b"PE\0\0");
        let opt = 0x80 + 24;
        f[opt..opt + 2].copy_from_slice(&0x20bu16.to_le_bytes());
        let mut f = with_trailer(f, r#"{"v":1,"build_id":"b1"}"#);
        let cert_at = f.len() as u32;
        let dir = opt + 112 + 32;
        f[dir..dir + 4].copy_from_slice(&cert_at.to_le_bytes());
        f.extend_from_slice(&[0xAAu8; 64]); // the signature
        let c = read_from(&mut Cursor::new(f)).unwrap().unwrap();
        assert_eq!(c.build_id, "b1");
    }
}
