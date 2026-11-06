//! Signed updates from the server (docs/client-builder.md, "For the agent: updates").
//!
//! The service asks the server for an update after it starts, every six hours and when the
//! server says `update_available`. It only installs a file when:
//!
//! - the manifest is signed with one of the Ed25519 keys this agent trusts (from the
//!   client builder, or handed out at enrollment over TLS),
//! - the manifest is not expired, names this device, this product, OS and architecture,
//! - the version is newer, or older with `allow_downgrade` (a pinned version),
//! - the downloaded file has exactly the size and SHA-256 of the manifest.
//!
//! The file is downloaded from the server the agent talks to (never another host), so the
//! device token is not sent anywhere else. Installing swaps the executable and restarts the
//! service through a small helper; when the new version does not start, the helper puts the
//! old one back (see `win::update`).
// The service (Windows) drives the updater; elsewhere only the tests use it.
#![cfg_attr(not(windows), allow(dead_code))]

use std::cmp::Ordering;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result, anyhow, bail, ensure};
use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use rhis_core::net::ServerConfig;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tracing::{info, warn};

use crate::config::AgentConfig;

/// Prefix of every signed manifest, so a signature can not be reused for anything else.
pub const SIGN_CONTEXT: &[u8] = b"rhis-agent-update-v1\0";
/// The product name in manifests for this program.
pub const PRODUCT: &str = "rhis-agent";
/// Upper limit for an update file.
pub const MAX_SIZE: u64 = 256 << 20;
/// Where files are fetched from on the server.
const FILES_PATH: &str = "/api/agent/v1/files/";

#[derive(Deserialize)]
struct Signed {
    manifest: String,
    signature: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct Manifest {
    pub product: String,
    pub version: String,
    pub os: String,
    pub arch: String,
    pub size: u64,
    pub sha256: String,
    pub url: String,
    pub expires_at: String,
    pub device_id: i64,
    #[serde(default)]
    pub allow_downgrade: bool,
}

/// Decodes base64 Ed25519 public keys, skipping anything that is not one.
pub fn decode_keys(keys: &[String]) -> Vec<VerifyingKey> {
    keys.iter()
        .filter_map(|k| {
            let b: [u8; 32] = B64.decode(k.trim()).ok()?.try_into().ok()?;
            VerifyingKey::from_bytes(&b).ok()
        })
        .collect()
}

/// Checks the signature and the expiry, like `agentupdate.Verify` on the server.
pub fn verify(
    manifest_b64: &str,
    signature_b64: &str,
    keys: &[VerifyingKey],
    now: time::OffsetDateTime,
) -> Result<Manifest> {
    let bytes = B64.decode(manifest_b64).context("bad manifest encoding")?;
    let sig = B64
        .decode(signature_b64)
        .context("bad signature encoding")?;
    let sig = Signature::from_slice(&sig).map_err(|_| anyhow!("bad signature"))?;
    let mut msg = SIGN_CONTEXT.to_vec();
    msg.extend_from_slice(&bytes);
    ensure!(
        keys.iter().any(|k| k.verify(&msg, &sig).is_ok()),
        "the update is not signed with a key this agent trusts"
    );
    let m: Manifest = serde_json::from_slice(&bytes).context("bad manifest")?;
    let exp = time::OffsetDateTime::parse(
        &m.expires_at,
        &time::format_description::well_known::Rfc3339,
    )
    .context("bad expiry in the manifest")?;
    ensure!(exp > now, "the update offer has expired");
    Ok(m)
}

/// What this agent is, to match a manifest against.
#[derive(Debug, Clone)]
pub struct Target {
    pub os: &'static str,
    pub arch: &'static str,
    pub device_id: i64,
    pub version: String,
}

impl Target {
    pub fn this(device_id: i64) -> Self {
        Self {
            os: "windows",
            arch: if cfg!(target_arch = "aarch64") {
                "arm64"
            } else {
                "amd64"
            },
            device_id,
            version: rhis_core::VERSION.to_string(),
        }
    }
}

/// Whether a verified manifest should be installed. `Ok(false)` means there is nothing to
/// do (same or older version); an error means the manifest is not for this agent.
pub fn accept(m: &Manifest, t: &Target) -> Result<bool> {
    ensure!(m.product == PRODUCT, "the update is for {}", m.product);
    ensure!(
        m.os == t.os && m.arch == t.arch,
        "the update is for {}/{}",
        m.os,
        m.arch
    );
    ensure!(
        m.device_id == t.device_id,
        "the update was issued to another device"
    );
    ensure!(
        m.size > 0 && m.size <= MAX_SIZE,
        "the update has a bad size"
    );
    ensure!(
        m.sha256.len() == 64 && m.sha256.bytes().all(|b| b.is_ascii_hexdigit()),
        "the update has a bad checksum"
    );
    ensure!(
        parse_version(&m.version).is_some(),
        "bad version {}",
        m.version
    );
    Ok(match compare_versions(&m.version, &t.version) {
        Ordering::Greater => true,
        Ordering::Less => m.allow_downgrade,
        Ordering::Equal => false,
    })
}

type Version<'a> = ([u64; 3], Vec<&'a str>);

fn parse_version(v: &str) -> Option<Version<'_>> {
    let v = v.trim().trim_start_matches('v');
    let v = v.split('+').next()?;
    let (core, pre) = match v.split_once('-') {
        Some((c, p)) => (c, p.split('.').collect()),
        None => (v, vec![]),
    };
    let mut n = [0u64; 3];
    let mut parts = core.split('.');
    for x in &mut n {
        *x = parts.next()?.parse().ok()?;
    }
    if parts.next().is_some() {
        return None;
    }
    Some((n, pre))
}

/// Compares two semantic versions ("v1.2.3", "1.2.3-test.4"); a pre-release is older than
/// the release itself. Versions that do not parse sort first.
pub fn compare_versions(a: &str, b: &str) -> Ordering {
    let (Some((an, ap)), Some((bn, bp))) = (parse_version(a), parse_version(b)) else {
        return parse_version(a).is_some().cmp(&parse_version(b).is_some());
    };
    an.cmp(&bn)
        .then_with(|| match (ap.is_empty(), bp.is_empty()) {
            (true, true) => Ordering::Equal,
            (true, false) => Ordering::Greater,
            (false, true) => Ordering::Less,
            (false, false) => {
                for (x, y) in ap.iter().zip(&bp) {
                    let o = match (x.parse::<u64>(), y.parse::<u64>()) {
                        (Ok(x), Ok(y)) => x.cmp(&y),
                        (Ok(_), Err(_)) => Ordering::Less,
                        (Err(_), Ok(_)) => Ordering::Greater,
                        _ => x.cmp(y),
                    };
                    if o != Ordering::Equal {
                        return o;
                    }
                }
                ap.len().cmp(&bp.len())
            }
        })
}

fn sha256_hex(b: &[u8]) -> String {
    hex::encode(Sha256::digest(b))
}

// ---- talking to the server ----

/// Asks the server for an update. `None`: nothing to do.
pub async fn check(
    server: &ServerConfig,
    token: &str,
    channel: &str,
    keys: &[VerifyingKey],
    t: &Target,
) -> Result<Option<Manifest>> {
    let url = format!(
        "{}?product=agent&os={}&arch={}&version=v{}&channel={}",
        server.https("/api/agent/v1/update"),
        t.os,
        t.arch,
        t.version.trim_start_matches('v'),
        if channel == "test" {
            "test"
        } else {
            "production"
        }
    );
    let resp = server
        .http_client()?
        .get(url)
        .bearer_auth(token)
        .send()
        .await?;
    match resp.status().as_u16() {
        204 => return Ok(None),
        200 => {}
        401 => bail!("the server refused this device's token"),
        s => bail!("update check failed: HTTP {s}"),
    }
    let s: Signed = resp.json().await.context("bad update answer")?;
    let m = verify(
        &s.manifest,
        &s.signature,
        keys,
        time::OffsetDateTime::now_utc(),
    )?;
    Ok(accept(&m, t)?.then_some(m))
}

/// The path on `server` to fetch a manifest's file from. Only the path of the manifest URL
/// is used: the file comes from the server the agent already trusts, with the same TLS
/// settings, and the device token never goes to another host. The SHA-256 is signed, so
/// where the bytes come from does not matter for their integrity.
fn file_path(m: &Manifest) -> Result<String> {
    let path = match m.url.find("://") {
        Some(i) => {
            let rest = &m.url[i + 3..];
            &rest[rest.find('/').unwrap_or(rest.len())..]
        }
        None => m.url.as_str(),
    };
    let path = path.split(['?', '#']).next().unwrap_or_default();
    ensure!(
        path == format!("{FILES_PATH}{}", m.sha256.to_ascii_lowercase()),
        "unexpected update address {}",
        m.url
    );
    Ok(path.to_string())
}

/// Downloads the file of a manifest into `dir` and checks size and SHA-256 while it arrives.
pub async fn download(
    server: &ServerConfig,
    token: &str,
    m: &Manifest,
    dir: &Path,
) -> Result<PathBuf> {
    std::fs::create_dir_all(dir)?;
    let name = format!("rhis-agent-{}.exe", m.version.trim_start_matches('v'));
    let dest = dir.join(&name);
    if std::fs::read(&dest)
        .is_ok_and(|b| b.len() as u64 == m.size && sha256_hex(&b).eq_ignore_ascii_case(&m.sha256))
    {
        return Ok(dest); // downloaded before
    }
    let mut resp = server
        .http_client()?
        .get(server.https(&file_path(m)?))
        .bearer_auth(token)
        .timeout(Duration::from_secs(600))
        .send()
        .await?;
    ensure!(
        resp.status().is_success(),
        "download failed: HTTP {}",
        resp.status().as_u16()
    );
    let mut body = Vec::with_capacity(m.size as usize);
    let mut hash = Sha256::new();
    while let Some(chunk) = resp.chunk().await? {
        ensure!(
            body.len() as u64 + chunk.len() as u64 <= m.size,
            "the download is larger than announced"
        );
        hash.update(&chunk);
        body.extend_from_slice(&chunk);
    }
    ensure!(
        body.len() as u64 == m.size,
        "the download has the wrong size"
    );
    ensure!(
        hex::encode(hash.finalize()).eq_ignore_ascii_case(&m.sha256),
        "the download does not match its checksum"
    );
    let tmp = dir.join(format!("{name}.part"));
    std::fs::write(&tmp, &body)?;
    std::fs::rename(&tmp, &dest)?;
    Ok(dest)
}

/// Tells the server how an update went: "downloaded", "installed" or "failed".
pub async fn report(
    server: &ServerConfig,
    token: &str,
    from: &str,
    to: &str,
    status: &str,
    error: &str,
) -> Result<()> {
    let v = |s: &str| format!("v{}", s.trim_start_matches('v'));
    let resp = server
        .http_client()?
        .post(server.https("/api/agent/v1/update/report"))
        .bearer_auth(token)
        .json(&serde_json::json!({
            "from_version": v(from), "to_version": v(to), "status": status, "error": error,
        }))
        .send()
        .await?;
    ensure!(
        resp.status().is_success(),
        "report failed: HTTP {}",
        resp.status().as_u16()
    );
    Ok(())
}

// ---- state across the restart ----

/// An update that was put in place and is waiting for the new version to confirm it.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default)]
pub struct Pending {
    pub from_version: String,
    pub to_version: String,
    /// The verified file that was installed.
    pub file: PathBuf,
    /// The executable before the update, kept until the new version runs.
    pub backup: PathBuf,
    /// "applied" (files swapped), "started" (the new version runs) or "rolled_back".
    pub state: String,
    pub error: String,
    /// Unix seconds when the update was applied.
    pub at: u64,
}

pub fn updates_dir(cfg_path: &Path) -> PathBuf {
    cfg_path
        .parent()
        .unwrap_or_else(|| Path::new("."))
        .join("updates")
}

fn pending_path(cfg_path: &Path) -> PathBuf {
    updates_dir(cfg_path).join("pending.json")
}

fn failed_path(cfg_path: &Path) -> PathBuf {
    updates_dir(cfg_path).join("failed.json")
}

pub fn read_pending(cfg_path: &Path) -> Option<Pending> {
    serde_json::from_slice(&std::fs::read(pending_path(cfg_path)).ok()?).ok()
}

pub fn write_pending(cfg_path: &Path, p: &Pending) -> Result<()> {
    let path = pending_path(cfg_path);
    std::fs::create_dir_all(path.parent().unwrap())?;
    let tmp = path.with_extension("tmp");
    std::fs::write(&tmp, serde_json::to_vec_pretty(p)?)?;
    std::fs::rename(&tmp, &path)?;
    Ok(())
}

pub fn now_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// How long a version that failed to install is skipped.
const FAILED_SKIP: u64 = 7 * 86400;

/// Versions that failed to install, with the time they failed.
fn failed_versions(cfg_path: &Path) -> std::collections::BTreeMap<String, u64> {
    std::fs::read(failed_path(cfg_path))
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

fn remember_failed(cfg_path: &Path, version: &str) {
    let mut f = failed_versions(cfg_path);
    let now = now_secs();
    f.retain(|_, t| now.saturating_sub(*t) < FAILED_SKIP);
    f.insert(version.trim_start_matches('v').to_string(), now);
    if let Ok(b) = serde_json::to_vec(&f) {
        let _ = std::fs::write(failed_path(cfg_path), b);
    }
}

fn recently_failed(cfg_path: &Path, version: &str) -> bool {
    failed_versions(cfg_path)
        .get(version.trim_start_matches('v'))
        .is_some_and(|t| now_secs().saturating_sub(*t) < FAILED_SKIP)
}

/// Called when the service starts: marks a just-applied update as running, so the helper
/// that restarted the service does not roll it back.
pub fn mark_started(cfg_path: &Path) {
    if let Some(mut p) = read_pending(cfg_path)
        && p.state == "applied"
        && compare_versions(&p.to_version, rhis_core::VERSION) == Ordering::Equal
    {
        p.state = "started".into();
        if let Err(e) = write_pending(cfg_path, &p) {
            warn!("cannot record the update: {e:#}");
        }
    }
}

/// What to tell the server about an update from before the restart, if anything.
#[derive(Debug, PartialEq)]
pub enum Outcome {
    Installed,
    Failed(String),
    /// Still in progress (the helper may still be working).
    Wait,
}

pub fn outcome(p: &Pending, running: &str, now: u64) -> Outcome {
    let is_new = compare_versions(&p.to_version, running) == Ordering::Equal;
    match p.state.as_str() {
        "started" if is_new => Outcome::Installed,
        "rolled_back" => Outcome::Failed(if p.error.is_empty() {
            "the new version did not start".into()
        } else {
            p.error.clone()
        }),
        _ if is_new => Outcome::Installed,
        // Not running the new version, and the helper did not finish within ten minutes.
        _ if now.saturating_sub(p.at) > 600 => {
            Outcome::Failed("the update was not completed".into())
        }
        _ => Outcome::Wait,
    }
}

// ---- the background task ----

/// Puts a verified update in place (the file's bytes) and returns the pending record to
/// keep. The caller then stops the service, so the helper can restart it.
pub type Apply = Box<dyn Fn(&[u8], &Pending) -> Result<Pending> + Send + Sync>;

pub struct Updater {
    pub cfg: AgentConfig,
    pub cfg_path: PathBuf,
    pub token: String,
    /// Set by `update_available` from the server.
    pub wake: Arc<tokio::sync::Notify>,
    /// Sessions in progress; an update waits until they are done.
    pub active: Arc<std::sync::Mutex<std::collections::HashSet<String>>>,
    /// Notified once the files are swapped; the service then stops itself.
    pub restart: Arc<tokio::sync::Notify>,
    pub apply: Apply,
}

impl Updater {
    fn servers(&self) -> Vec<ServerConfig> {
        crate::agent::servers(&self.cfg).unwrap_or_default()
    }

    /// Reports the result of an update from before the restart.
    async fn finish_pending(&self) {
        let Some(p) = read_pending(&self.cfg_path) else {
            return;
        };
        let (status, error) = match outcome(&p, rhis_core::VERSION, now_secs()) {
            Outcome::Wait => return,
            Outcome::Installed => ("installed", String::new()),
            Outcome::Failed(e) => ("failed", e),
        };
        let mut sent = false;
        for s in self.servers() {
            match report(
                &s,
                &self.token,
                &p.from_version,
                &p.to_version,
                status,
                &error,
            )
            .await
            {
                Ok(()) => {
                    sent = true;
                    break;
                }
                Err(e) => warn!("update report to {}: {e:#}", s.url),
            }
        }
        if !sent {
            return; // try again with the next check
        }
        if status == "installed" {
            info!("update to {} installed", p.to_version);
            let _ = std::fs::remove_file(&p.backup);
        } else {
            warn!("update to {} failed: {error}", p.to_version);
            remember_failed(&self.cfg_path, &p.to_version);
        }
        let _ = std::fs::remove_file(&p.file);
        let _ = std::fs::remove_file(pending_path(&self.cfg_path));
    }

    /// One check: ask each server in turn until one answers.
    async fn check_once(&self) -> Result<bool> {
        self.finish_pending().await;
        if read_pending(&self.cfg_path).is_some() {
            return Ok(false); // the last update is not settled yet
        }
        let keys = decode_keys(self.cfg.update_keys());
        if keys.is_empty() {
            return Ok(false);
        }
        let t = Target::this(self.cfg.device_id);
        let mut last = None;
        for s in self.servers() {
            match check(&s, &self.token, self.cfg.update_channel(), &keys, &t).await {
                Ok(None) => return Ok(false),
                Ok(Some(m)) => return self.install(&s, m).await,
                Err(e) => last = Some(e.context(format!("{}", s.url))),
            }
        }
        Err(last.unwrap_or_else(|| anyhow!("no server")))
    }

    async fn install(&self, s: &ServerConfig, m: Manifest) -> Result<bool> {
        let from = rhis_core::VERSION;
        if recently_failed(&self.cfg_path, &m.version) {
            return Ok(false);
        }
        info!("update {} available, downloading", m.version);
        let file = match download(s, &self.token, &m, &updates_dir(&self.cfg_path)).await {
            Ok(f) => f,
            Err(e) => {
                let _ = report(
                    s,
                    &self.token,
                    from,
                    &m.version,
                    "failed",
                    &format!("{e:#}"),
                )
                .await;
                return Err(e);
            }
        };
        let _ = report(s, &self.token, from, &m.version, "downloaded", "").await;
        // Do not cut off the Ctrl+Alt+Del support of running sessions.
        while !self.active.lock().unwrap().is_empty() {
            tokio::time::sleep(Duration::from_secs(60)).await;
        }
        let p = Pending {
            from_version: from.to_string(),
            to_version: m.version.clone(),
            file: file.clone(),
            state: "applied".into(),
            at: now_secs(),
            ..Default::default()
        };
        // Check the file once more right before it is used: it sat on disk meanwhile.
        let bytes = std::fs::read(&file)?;
        if bytes.len() as u64 != m.size || !sha256_hex(&bytes).eq_ignore_ascii_case(&m.sha256) {
            let _ = std::fs::remove_file(&file);
            bail!("the downloaded update changed on disk");
        }
        match (self.apply)(&bytes, &p) {
            Ok(p) => {
                write_pending(&self.cfg_path, &p)?;
                info!("update {} in place, restarting", m.version);
                self.restart.notify_one();
                Ok(true)
            }
            Err(e) => {
                let msg = format!("{e:#}");
                let _ = report(s, &self.token, from, &m.version, "failed", &msg).await;
                remember_failed(&self.cfg_path, &m.version);
                Err(e)
            }
        }
    }

    /// Runs until an update was applied.
    pub async fn run(self: Arc<Self>) {
        if decode_keys(self.cfg.update_keys()).is_empty() {
            info!(
                "automatic updates are off: this agent has no update key (enroll it again to get one)"
            );
        }
        let jitter = |max: u64| Duration::from_secs(rand::random::<u64>() % max.max(1));
        let mut next = Duration::from_secs(30) + jitter(90);
        loop {
            tokio::select! {
                _ = tokio::time::sleep(next) => {}
                _ = self.wake.notified() => {
                    // Spread the load when the server tells every agent at once.
                    tokio::time::sleep(jitter(120)).await;
                }
            }
            match self.check_once().await {
                Ok(true) => return,
                Ok(false) => {}
                Err(e) => warn!("update check: {e:#}"),
            }
            next = Duration::from_secs(6 * 3600) + jitter(1800);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signer, SigningKey};

    pub(super) fn sign(key: &SigningKey, manifest: &str) -> (String, String) {
        let mut msg = SIGN_CONTEXT.to_vec();
        msg.extend_from_slice(manifest.as_bytes());
        (B64.encode(manifest), B64.encode(key.sign(&msg).to_bytes()))
    }

    fn manifest(version: &str, device: i64, downgrade: bool) -> String {
        format!(
            r#"{{"product":"rhis-agent","version":"{version}","os":"windows","arch":"amd64","size":5,"sha256":"{}","url":"https://a.example/api/agent/v1/files/{}","released_at":"2026-11-01T00:00:00Z","issued_at":"2026-11-03T10:00:00Z","expires_at":"2026-11-04T10:00:00Z","device_id":{device},"allow_downgrade":{downgrade}}}"#,
            "ab".repeat(32),
            "ab".repeat(32)
        )
    }

    fn target(version: &str) -> Target {
        Target {
            os: "windows",
            arch: "amd64",
            device_id: 12,
            version: version.into(),
        }
    }

    #[test]
    fn versions() {
        use Ordering::*;
        assert_eq!(compare_versions("v0.3.0", "0.2.0"), Greater);
        assert_eq!(compare_versions("0.2.0", "v0.2.0"), Equal);
        assert_eq!(compare_versions("0.10.0", "0.9.9"), Greater);
        assert_eq!(compare_versions("0.2.0", "0.2.0-test.3"), Greater);
        assert_eq!(compare_versions("0.2.0-test.10", "0.2.0-test.9"), Greater);
        assert_eq!(compare_versions("0.2.0-test.1", "0.2.0-test"), Greater);
        assert_eq!(compare_versions("0.2.0-alpha", "0.2.0-1"), Greater);
        assert_eq!(compare_versions("0.2.0+build.5", "0.2.0"), Equal);
        assert_eq!(compare_versions("junk", "0.0.1"), Less);
        assert!(parse_version("1.2").is_none() && parse_version("1.2.3.4").is_none());
    }

    #[test]
    fn verifies_like_the_server() {
        let key = SigningKey::from_bytes(&[7; 32]);
        let other = SigningKey::from_bytes(&[8; 32]);
        let keys = decode_keys(&[B64.encode(key.verifying_key().to_bytes()), "junk".into()]);
        assert_eq!(keys.len(), 1);
        let now = time::macros::datetime!(2026-11-03 10:00 UTC);
        let m = manifest("v0.3.0", 12, false);
        let (mb, sb) = sign(&key, &m);
        let got = verify(&mb, &sb, &keys, now).unwrap();
        assert_eq!((got.version.as_str(), got.device_id), ("v0.3.0", 12));
        // Another key, a changed manifest, an expired offer.
        let (mb2, sb2) = sign(&other, &m);
        assert!(verify(&mb2, &sb2, &keys, now).is_err());
        assert!(verify(&B64.encode(m.replace("v0.3.0", "v9.9.9")), &sb, &keys, now).is_err());
        let later = time::macros::datetime!(2026-11-05 10:00 UTC);
        assert!(verify(&mb, &sb, &keys, later).is_err());
        assert!(verify(&mb, &sb, &[], now).is_err());
    }

    #[test]
    fn accepts_only_what_fits() {
        let parse = |s: &str| serde_json::from_str::<Manifest>(s).unwrap();
        assert!(accept(&parse(&manifest("v0.3.0", 12, false)), &target("0.2.0")).unwrap());
        // Same version, older without and with a pinned downgrade.
        assert!(!accept(&parse(&manifest("v0.2.0", 12, true)), &target("0.2.0")).unwrap());
        assert!(!accept(&parse(&manifest("v0.1.0", 12, false)), &target("0.2.0")).unwrap());
        assert!(accept(&parse(&manifest("v0.1.0", 12, true)), &target("0.2.0")).unwrap());
        // Another device, product or architecture.
        assert!(accept(&parse(&manifest("v0.3.0", 13, false)), &target("0.2.0")).is_err());
        let quick = manifest("v0.3.0", 12, false).replace("rhis-agent", "rhis-quick");
        assert!(accept(&parse(&quick), &target("0.2.0")).is_err());
        let arm = manifest("v0.3.0", 12, false).replace("amd64", "arm64");
        assert!(accept(&parse(&arm), &target("0.2.0")).is_err());
    }

    #[test]
    fn file_comes_from_the_same_server() {
        let mut m: Manifest = serde_json::from_str(&manifest("v0.3.0", 12, false)).unwrap();
        let want = format!("{FILES_PATH}{}", "ab".repeat(32));
        assert_eq!(file_path(&m).unwrap(), want);
        m.url = format!("https://elsewhere.example:8443{want}?x=1");
        assert_eq!(file_path(&m).unwrap(), want);
        m.url = "https://a.example/other".into();
        assert!(file_path(&m).is_err());
        m.url = format!("https://a.example{FILES_PATH}{}", "cd".repeat(32));
        assert!(file_path(&m).is_err());
    }

    #[test]
    fn outcomes() {
        let p = |state: &str, at| Pending {
            from_version: "0.2.0".into(),
            to_version: "v0.3.0".into(),
            state: state.into(),
            at,
            ..Default::default()
        };
        assert_eq!(outcome(&p("started", 0), "0.3.0", 10), Outcome::Installed);
        assert_eq!(outcome(&p("applied", 0), "0.3.0", 10), Outcome::Installed);
        assert!(matches!(
            outcome(&p("rolled_back", 0), "0.2.0", 10),
            Outcome::Failed(_)
        ));
        assert_eq!(outcome(&p("applied", 0), "0.2.0", 100), Outcome::Wait);
        assert!(matches!(
            outcome(&p("applied", 0), "0.2.0", 1000),
            Outcome::Failed(_)
        ));
    }

    /// A tiny HTTP server on loopback that answers like the update endpoints.
    async fn fake_server(
        offer: Option<(String, String)>,
        file: Vec<u8>,
    ) -> (ServerConfig, Arc<std::sync::Mutex<Vec<String>>>) {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = l.local_addr().unwrap();
        let log = Arc::new(std::sync::Mutex::new(Vec::new()));
        let log2 = log.clone();
        tokio::spawn(async move {
            loop {
                let (mut c, _) = l.accept().await.unwrap();
                let (offer, file, log) = (offer.clone(), file.clone(), log2.clone());
                tokio::spawn(async move {
                    let mut buf = Vec::new();
                    let mut tmp = [0u8; 4096];
                    // Read headers and a small body.
                    loop {
                        let n = c.read(&mut tmp).await.unwrap();
                        buf.extend_from_slice(&tmp[..n]);
                        let text = String::from_utf8_lossy(&buf);
                        if let Some(i) = text.find("\r\n\r\n") {
                            let len = text
                                .lines()
                                .find_map(|l| {
                                    l.to_ascii_lowercase()
                                        .strip_prefix("content-length: ")
                                        .map(|v| v.trim().parse::<usize>().unwrap())
                                })
                                .unwrap_or(0);
                            if buf.len() >= i + 4 + len || n == 0 {
                                break;
                            }
                        }
                    }
                    let text = String::from_utf8_lossy(&buf).to_string();
                    let line = text.lines().next().unwrap().to_string();
                    let authed = text.contains("authorization: Bearer tok")
                        || text.contains("Authorization: Bearer tok");
                    log.lock().unwrap().push(format!(
                        "{line} {}",
                        text.split("\r\n\r\n").nth(1).unwrap_or("")
                    ));
                    let (status, body): (&str, Vec<u8>) = if !authed {
                        ("401 Unauthorized", vec![])
                    } else if line.starts_with("GET /api/agent/v1/update?") {
                        match &offer {
                            Some((m, s)) => (
                                "200 OK",
                                format!(r#"{{"manifest":"{m}","signature":"{s}"}}"#).into_bytes(),
                            ),
                            None => ("204 No Content", vec![]),
                        }
                    } else if line.starts_with("GET /api/agent/v1/files/") {
                        ("200 OK", file)
                    } else if line.starts_with("POST /api/agent/v1/update/report") {
                        ("204 No Content", vec![])
                    } else {
                        ("404 Not Found", vec![])
                    };
                    let head = format!(
                        "HTTP/1.1 {status}\r\nContent-Length: {}\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n",
                        body.len()
                    );
                    let _ = c.write_all(head.as_bytes()).await;
                    let _ = c.write_all(&body).await;
                });
            }
        });
        let s = ServerConfig::new(&format!("http://{addr}"), None, true).unwrap();
        (s, log)
    }

    fn real_manifest(file: &[u8], version: &str) -> String {
        let sum = sha256_hex(file);
        let exp = (time::OffsetDateTime::now_utc() + time::Duration::hours(1))
            .format(&time::format_description::well_known::Rfc3339)
            .unwrap();
        format!(
            r#"{{"product":"rhis-agent","version":"{version}","os":"windows","arch":"amd64","size":{},"sha256":"{sum}","url":"https://public.example/api/agent/v1/files/{sum}","expires_at":"{exp}","device_id":12,"allow_downgrade":false}}"#,
            file.len()
        )
    }

    #[tokio::test]
    async fn checks_downloads_and_reports() {
        let key = SigningKey::from_bytes(&[7; 32]);
        let keys = decode_keys(&[B64.encode(key.verifying_key().to_bytes())]);
        let file = b"MZ new agent".to_vec();
        let (mb, sb) = sign(&key, &real_manifest(&file, "v0.3.0"));
        let (srv, log) = fake_server(Some((mb, sb)), file.clone()).await;
        let t = target("0.2.0");
        let m = check(&srv, "tok", "production", &keys, &t)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(m.version, "v0.3.0");
        let dir = std::env::temp_dir().join(format!("rhis-upd-{}", rand::random::<u32>()));
        let path = download(&srv, "tok", &m, &dir).await.unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), file);
        report(&srv, "tok", "0.2.0", "v0.3.0", "installed", "")
            .await
            .unwrap();
        let log = log.lock().unwrap().join("\n");
        // The file came from our server (not public.example), the report names both versions.
        assert!(log.contains("GET /api/agent/v1/update?product=agent&os=windows&arch=amd64&version=v0.2.0&channel=production"), "{log}");
        assert!(
            log.contains(&format!("GET /api/agent/v1/files/{}", m.sha256)),
            "{log}"
        );
        assert!(
            log.contains(r#""from_version":"v0.2.0""#) && log.contains(r#""status":"installed""#),
            "{log}"
        );
        // Wrong token.
        assert!(check(&srv, "nope", "production", &keys, &t).await.is_err());
        let _ = std::fs::remove_dir_all(dir);
    }

    #[tokio::test]
    async fn refuses_a_tampered_download() {
        let key = SigningKey::from_bytes(&[7; 32]);
        let keys = decode_keys(&[B64.encode(key.verifying_key().to_bytes())]);
        let good = b"MZ new agent".to_vec();
        let (mb, sb) = sign(&key, &real_manifest(&good, "v0.3.0"));
        // The server (or something in between) hands out other bytes of the same length.
        let (srv, _) = fake_server(Some((mb, sb)), b"MZ bad agent".to_vec()).await;
        let m = check(&srv, "tok", "test", &keys, &target("0.2.0"))
            .await
            .unwrap()
            .unwrap();
        let dir = std::env::temp_dir().join(format!("rhis-upd-{}", rand::random::<u32>()));
        let err = download(&srv, "tok", &m, &dir).await.unwrap_err();
        assert!(err.to_string().contains("checksum"), "{err}");
        assert!(!dir.join("rhis-agent-0.3.0.exe").exists());
        // A signature from another key is refused before anything is downloaded.
        let other = SigningKey::from_bytes(&[9; 32]);
        let (mb, sb) = sign(&other, &real_manifest(&good, "v0.3.0"));
        let (srv, _) = fake_server(Some((mb, sb)), good).await;
        assert!(
            check(&srv, "tok", "test", &keys, &target("0.2.0"))
                .await
                .is_err()
        );
        // Nothing offered.
        let (srv, _) = fake_server(None, vec![]).await;
        assert!(
            check(&srv, "tok", "test", &keys, &target("0.2.0"))
                .await
                .unwrap()
                .is_none()
        );
        let _ = std::fs::remove_dir_all(dir);
    }

    /// Against a running server: `RHIS_E2E_AGENT_JSON=/path/agent.json cargo test -p rhis-agent
    /// -- --ignored e2e_server`. The agent must be enrolled (with `--insecure-dev` for a local
    /// http server) and the server must offer it an update.
    #[tokio::test]
    #[ignore]
    async fn e2e_server() {
        let path = PathBuf::from(std::env::var("RHIS_E2E_AGENT_JSON").unwrap());
        let cfg = AgentConfig::load(&path).unwrap().unwrap();
        let keys = decode_keys(cfg.update_keys());
        assert!(
            !keys.is_empty(),
            "the agent got no update key at enrollment"
        );
        let srv = crate::agent::servers(&cfg).unwrap().remove(0);
        let token = cfg.token().unwrap();
        let t = Target::this(cfg.device_id);
        let m = check(&srv, &token, cfg.update_channel(), &keys, &t)
            .await
            .unwrap()
            .expect("the server offers no update");
        let dir = updates_dir(&path);
        let f = download(&srv, &token, &m, &dir).await.unwrap();
        assert_eq!(std::fs::metadata(&f).unwrap().len(), m.size);
        report(
            &srv,
            &token,
            rhis_core::VERSION,
            &m.version,
            "downloaded",
            "",
        )
        .await
        .unwrap();
        println!("offered and downloaded {} ({} bytes)", m.version, m.size);
    }
}
