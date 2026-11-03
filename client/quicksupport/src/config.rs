// SPDX-License-Identifier: AGPL-3.0-only
//! Where the client finds its server and branding.
//!
//! Order, first match wins:
//! 1. `--server <url>` on the command line (support staff, testing).
//! 2. A config block appended to the exe by the server's download endpoint or
//!    the client builder: `<json><u32 little-endian length>RHISQS01`.
//! 3. `rhis-quicksupport.json` next to the exe.
//! 4. The `RHIS_SERVER_URL` value baked in at compile time.
//!
//! Only `https://` is accepted, so every byte between client and server is
//! encrypted. `--insecure-dev` allows `http://` to a loopback address for
//! development and nothing else.

use serde::Deserialize;
use std::{fs, path::Path};

pub const TRAILER_MAGIC: &[u8; 8] = b"RHISQS01";

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct FileConfig {
    pub server: String,
    pub product_name: String,
    pub company_name: String,
}

#[derive(Debug, Clone)]
pub struct Config {
    pub server: String,
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

        let server = cli_server
            .or_else(|| Some(file.server.clone()).filter(|s| !s.is_empty()))
            .or_else(|| option_env!("RHIS_SERVER_URL").map(str::to_string))
            .unwrap_or_default();

        let cfg = Config {
            server: server.trim_end_matches('/').to_string(),
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

pub fn parse_trailer(data: &[u8]) -> Option<FileConfig> {
    let n = data.len();
    if n < 12 || &data[n - 8..] != TRAILER_MAGIC {
        return None;
    }
    let len = u32::from_le_bytes(data[n - 12..n - 8].try_into().ok()?) as usize;
    if len == 0 || len > 64 * 1024 || len > n - 12 {
        return None;
    }
    serde_json::from_slice(&data[n - 12 - len..n - 12]).ok()
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
    fn trailer_roundtrip() {
        let json = br#"{"server":"https://remote.rhis.nl","company_name":"RHIS"}"#;
        let mut exe = b"MZ fake exe body".to_vec();
        exe.extend_from_slice(json);
        exe.extend_from_slice(&(json.len() as u32).to_le_bytes());
        exe.extend_from_slice(TRAILER_MAGIC);
        let c = parse_trailer(&exe).unwrap();
        assert_eq!(c.server, "https://remote.rhis.nl");
        assert_eq!(c.company_name, "RHIS");
        assert!(parse_trailer(b"MZ no trailer").is_none());
    }
}
