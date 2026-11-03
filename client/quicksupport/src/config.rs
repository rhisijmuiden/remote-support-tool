// SPDX-License-Identifier: AGPL-3.0-only
//! Where the client finds its server and branding.
//!
//! Order, first match wins:
//! 1. `--server <url>` on the command line (support staff, testing).
//! 2. The configuration the server (download link or client builder) appended
//!    to the exe, in the client builder format (docs/client-builder.md):
//!    `[zero padding][JSON][u32 little-endian length]["RHISCFG1"]`, directly
//!    before the Authenticode certificate table when the file is signed.
//! 3. `rhis-quicksupport.json` next to the exe (same JSON fields).
//! 4. The `RHIS_SERVER_URL` value baked in at compile time.
//!
//! Only `https://` is accepted, so every byte between client and server is
//! encrypted. `--insecure-dev` allows `http://` to a loopback address for
//! development and nothing else.

use serde::Deserialize;
use std::{fs, path::Path};

pub const TRAILER_MAGIC: &[u8; 8] = b"RHISCFG1";

/// The embedded configuration. Field names follow the client builder contract.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct FileConfig {
    /// Failover list, in order.
    pub servers: Vec<String>,
    /// Single server, accepted in the sidecar file for convenience.
    pub server: String,
    pub product_name: String,
    pub company_name: String,
    /// Base64 PNG.
    pub logo_png: String,
    /// "#RRGGBB"
    pub accent_color: String,
}

#[derive(Debug, Clone)]
pub struct Config {
    pub server: String,
    /// Further servers to try when `server` cannot be reached.
    pub alternates: Vec<String>,
    pub logo_png: Option<Vec<u8>>,
    pub accent: Option<[u8; 3]>,
    pub product_name: String,
    pub company_name: String,
    pub insecure_dev: bool,
    pub demo: bool,
    pub lang: Option<String>,
}

#[derive(Debug, PartialEq)]
pub enum ConfigError {
    NoServer,
    NotHttps(String),
    BadUrl(String),
}

impl Config {
    pub fn load(args: &[String]) -> Result<Config, ConfigError> {
        let mut cli_server = None;
        let mut insecure_dev = false;
        let mut demo = false;
        let mut lang = None;
        let mut it = args.iter().skip(1);
        while let Some(a) = it.next() {
            match a.as_str() {
                "--server" => cli_server = it.next().cloned(),
                "--insecure-dev" => insecure_dev = true,
                "--demo" => demo = true,
                "--lang" => lang = it.next().cloned(),
                _ => {}
            }
        }

        let exe = std::env::current_exe().ok();
        let file = exe
            .as_deref()
            .and_then(read_trailer)
            .or_else(|| exe.as_deref().and_then(read_sidecar))
            .unwrap_or_default();

        let mut servers: Vec<String> = match cli_server {
            Some(s) => vec![s],
            None => {
                let mut v = file.servers.clone();
                if !file.server.is_empty() {
                    v.insert(0, file.server.clone());
                }
                if v.is_empty() {
                    v.extend(option_env!("RHIS_SERVER_URL").map(str::to_string));
                }
                v
            }
        };
        servers
            .iter_mut()
            .for_each(|s| *s = s.trim().trim_end_matches('/').to_string());
        servers.retain(|s| !s.is_empty());
        let server = if servers.is_empty() {
            String::new()
        } else {
            servers.remove(0)
        };
        // Alternates must be https too; drop any that are not.
        servers.retain(|s| validate_server(s, insecure_dev).is_ok());

        use base64::Engine;
        let logo_png = base64::engine::general_purpose::STANDARD
            .decode(file.logo_png.trim())
            .ok()
            .filter(|b| b.starts_with(b"\x89PNG\r\n\x1a\n") && b.len() <= 256 * 1024);
        let accent = parse_color(&file.accent_color);

        let cfg = Config {
            server,
            alternates: servers,
            logo_png,
            accent,
            product_name: non_empty(file.product_name, "RHIS Remote"),
            company_name: non_empty(file.company_name, "RHIS"),
            insecure_dev,
            demo,
            lang,
        };
        if !cfg.demo {
            validate_server(&cfg.server, cfg.insecure_dev)?;
        }
        Ok(cfg)
    }
}

fn parse_color(s: &str) -> Option<[u8; 3]> {
    let h = s.trim().strip_prefix('#')?;
    if h.len() != 6 {
        return None;
    }
    let v = u32::from_str_radix(h, 16).ok()?;
    Some([(v >> 16) as u8, (v >> 8) as u8, v as u8])
}

fn non_empty(s: String, default: &str) -> String {
    if s.trim().is_empty() {
        default.to_string()
    } else {
        s
    }
}

/// Accepts https URLs, and http to loopback only with the development flag.
pub fn validate_server(url: &str, insecure_dev: bool) -> Result<(), ConfigError> {
    if url.is_empty() {
        return Err(ConfigError::NoServer);
    }
    let (scheme, rest) = url
        .split_once("://")
        .ok_or_else(|| ConfigError::BadUrl(url.to_string()))?;
    let host = rest.split(['/', '?', '#']).next().unwrap_or("");
    if host.is_empty() || host.contains('@') {
        return Err(ConfigError::BadUrl(url.to_string()));
    }
    match scheme.to_ascii_lowercase().as_str() {
        "https" => Ok(()),
        "http" if insecure_dev && is_loopback(host) => Ok(()),
        _ => Err(ConfigError::NotHttps(url.to_string())),
    }
}

fn is_loopback(host_port: &str) -> bool {
    let host = if let Some(h) = host_port.strip_prefix('[') {
        h.split(']').next().unwrap_or("")
    } else {
        host_port.rsplit_once(':').map_or(host_port, |(h, _)| h)
    };
    host.eq_ignore_ascii_case("localhost")
        || host
            .parse::<std::net::IpAddr>()
            .is_ok_and(|ip| ip.is_loopback())
}

fn read_trailer(exe: &Path) -> Option<FileConfig> {
    let data = fs::read(exe).ok()?;
    parse_trailer(&data)
}

/// Finds the trailer before the Authenticode certificate table (or at the end
/// of an unsigned file). Mirrors builder.Extract on the server.
pub fn parse_trailer(data: &[u8]) -> Option<FileConfig> {
    let end = security_dir_offset(data).unwrap_or(data.len());
    if end < 12 || end > data.len() || &data[end - 8..end] != TRAILER_MAGIC {
        return None;
    }
    let len = u32::from_le_bytes(data[end - 12..end - 8].try_into().ok()?) as usize;
    if len == 0 || len > 1024 * 1024 || len > end - 12 {
        return None;
    }
    serde_json::from_slice(&data[end - 12 - len..end - 12]).ok()
}

/// File offset of the PE certificate table (data directory 4), if the file is signed.
fn security_dir_offset(exe: &[u8]) -> Option<usize> {
    let u16_at = |p: usize| exe.get(p..p + 2).map(|b| u16::from_le_bytes([b[0], b[1]]));
    let u32_at = |p: usize| {
        exe.get(p..p + 4)
            .map(|b| u32::from_le_bytes([b[0], b[1], b[2], b[3]]))
    };
    if exe.get(0..2)? != b"MZ" {
        return None;
    }
    let pe = u32_at(0x3c)? as usize;
    if exe.get(pe..pe + 4)? != b"PE\0\0" {
        return None;
    }
    let opt = pe + 24;
    let (n_pos, dd) = match u16_at(opt)? {
        0x10b => (opt + 92, opt + 96),
        0x20b => (opt + 108, opt + 112),
        _ => return None,
    };
    if u32_at(n_pos)? < 5 {
        return None;
    }
    let off = u32_at(dd + 4 * 8)? as usize;
    let size = u32_at(dd + 4 * 8 + 4)?;
    (off > 0 && size > 0).then_some(off)
}

fn read_sidecar(exe: &Path) -> Option<FileConfig> {
    let p = exe.with_file_name("rhis-quicksupport.json");
    serde_json::from_slice(&fs::read(p).ok()?).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_https() {
        assert_eq!(validate_server("https://remote.example.com", false), Ok(()));
        assert!(validate_server("http://remote.example.com", false).is_err());
        assert!(validate_server("http://remote.example.com", true).is_err());
        assert_eq!(validate_server("http://127.0.0.1:8080", true), Ok(()));
        assert_eq!(validate_server("http://localhost:8080", true), Ok(()));
        assert_eq!(validate_server("http://[::1]:8080", true), Ok(()));
        assert!(validate_server("http://127.0.0.1:8080", false).is_err());
        assert!(validate_server("ftp://x", true).is_err());
        assert!(validate_server("https://user@evil", false).is_err());
        assert_eq!(validate_server("", false), Err(ConfigError::NoServer));
    }

    #[test]
    fn trailer_before_signature() {
        // Minimal PE32+ header with a certificate table after the trailer.
        let mut exe = vec![0u8; 0x200];
        exe[0..2].copy_from_slice(b"MZ");
        exe[0x3c..0x40].copy_from_slice(&0x40u32.to_le_bytes());
        exe[0x40..0x44].copy_from_slice(b"PE\0\0");
        let opt = 0x40 + 24;
        exe[opt..opt + 2].copy_from_slice(&0x20bu16.to_le_bytes());
        exe[opt + 108..opt + 112].copy_from_slice(&16u32.to_le_bytes());
        let json = br#"{"servers":["https://a.example","https://b.example"]}"#;
        exe.extend_from_slice(json);
        exe.extend_from_slice(&(json.len() as u32).to_le_bytes());
        exe.extend_from_slice(TRAILER_MAGIC);
        let cert_off = exe.len() as u32;
        exe.extend_from_slice(&[0xAB; 64]); // "signature"
        let dd = opt + 112 + 4 * 8;
        exe[dd..dd + 4].copy_from_slice(&cert_off.to_le_bytes());
        exe[dd + 4..dd + 8].copy_from_slice(&64u32.to_le_bytes());
        let c = parse_trailer(&exe).unwrap();
        assert_eq!(c.servers.len(), 2);
    }

    #[test]
    fn trailer_roundtrip() {
        let json =
            br#"{"v":1,"servers":["https://remote.rhis.nl"],"company_name":"RHIS","mode":"quick"}"#;
        let mut exe = b"MZ fake exe body".to_vec();
        exe.extend_from_slice(&[0, 0, 0]);
        exe.extend_from_slice(json);
        exe.extend_from_slice(&(json.len() as u32).to_le_bytes());
        exe.extend_from_slice(TRAILER_MAGIC);
        let c = parse_trailer(&exe).unwrap();
        assert_eq!(c.servers, vec!["https://remote.rhis.nl"]);
        assert_eq!(c.company_name, "RHIS");
        assert!(parse_trailer(b"MZ no trailer").is_none());
        assert_eq!(parse_color("#FF6B57"), Some([0xFF, 0x6B, 0x57]));
        assert_eq!(parse_color("red"), None);
    }
}
