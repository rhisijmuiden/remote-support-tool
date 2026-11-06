//! The agent's stored configuration (`agent.json`).
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

use crate::buildcfg::BuildConfig;
use crate::secret;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct AgentConfig {
    /// Server URLs in failover order; the first one that answers is used.
    pub servers: Vec<String>,
    /// Optional SHA-256 pin of the server certificate (hex).
    pub server_cert_sha256: String,
    pub machine_id: String,
    pub device_id: i64,
    pub device_code: String,
    /// The device token, protected with DPAPI (machine scope) on Windows.
    pub device_token: String,
    /// Settings from a branded build, kept so a later update without them still knows them.
    pub build: Option<BuildConfig>,
    /// Update keys handed out at enrollment, for agents without a branded build.
    pub update_keys: Vec<String>,
    /// Release channel handed out at enrollment ("production" or "test").
    pub channel: String,
    /// Development only: allow http:// to localhost.
    pub insecure_dev: bool,
}

impl AgentConfig {
    pub fn load(path: &Path) -> Result<Option<Self>> {
        match std::fs::read(path) {
            Ok(b) => Ok(Some(
                serde_json::from_slice(&b).with_context(|| format!("parse {}", path.display()))?,
            )),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(e).with_context(|| format!("read {}", path.display())),
        }
    }

    pub fn save(&self, path: &Path) -> Result<()> {
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
            secret::restrict_dir(dir)?;
        }
        let tmp = path.with_extension("json.tmp");
        std::fs::write(&tmp, serde_json::to_vec_pretty(self)?)?;
        secret::restrict_file(&tmp)?;
        std::fs::rename(&tmp, path)?;
        Ok(())
    }

    pub fn token(&self) -> Result<String> {
        secret::unprotect(&self.device_token)
    }

    pub fn set_token(&mut self, token: &str) -> Result<()> {
        self.device_token = secret::protect(token)?;
        Ok(())
    }

    /// The Ed25519 keys (base64) that sign updates for this agent.
    pub fn update_keys(&self) -> &[String] {
        match &self.build {
            Some(b) if !b.update_keys.is_empty() => &b.update_keys,
            _ => &self.update_keys,
        }
    }

    /// The release channel this agent follows.
    pub fn update_channel(&self) -> &str {
        let c = match &self.build {
            Some(b) if !b.channel.is_empty() => b.channel.as_str(),
            _ => self.channel.as_str(),
        };
        if c == "test" { "test" } else { "production" }
    }

    pub fn product_name(&self) -> String {
        self.build
            .as_ref()
            .map(|b| b.product_name.clone())
            .filter(|n| !n.is_empty())
            .unwrap_or_else(|| "RHIS Remote".into())
    }
}

/// Default location of agent.json.
pub fn default_path() -> PathBuf {
    #[cfg(windows)]
    {
        let base = std::env::var_os("ProgramData")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from(r"C:\ProgramData"));
        base.join("RHIS Remote").join("agent.json")
    }
    #[cfg(not(windows))]
    {
        PathBuf::from("/var/lib/rhis-agent/agent.json")
    }
}
