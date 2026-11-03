// SPDX-License-Identifier: AGPL-3.0-only
//! Updates from the Info dialog.
//!
//! A build from the client builder carries `update_keys`. It then only accepts
//! an update manifest signed with one of those Ed25519 keys, issued for this
//! device and not expired (the same rules as the agent, see
//! docs/client-builder.md). A plain build (no keys) asks the server which
//! QuickSupport it offers; that file comes over HTTPS and is checked against
//! the SHA-256 the server names.

use crate::backend::{VERSION, http, is_newer};
use crate::config::{Config, trailer_bytes, validate_server};
use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use serde::Deserialize;

/// What the window needs from the running connection to ask for updates.
#[derive(Clone, Debug)]
pub struct UpdateCtx {
    pub server: String,
    pub token: String,
    pub device_id: i64,
}

/// An update the user can install.
#[derive(Clone, Debug)]
pub struct Offer {
    pub version: String,
    pub url: String,
    pub sha256: String,
    pub size: Option<u64>,
    /// Device token, for the signed download endpoint.
    pub bearer: Option<String>,
}

#[derive(Deserialize)]
struct Signed {
    manifest: String,
    signature: String,
}

#[derive(Deserialize, Debug)]
pub struct Manifest {
    pub product: String,
    pub version: String,
    pub size: u64,
    pub sha256: String,
    pub url: String,
    pub expires_at: String,
    pub device_id: i64,
    #[serde(default)]
    pub allow_downgrade: bool,
}

const SIGN_CONTEXT: &[u8] = b"rhis-agent-update-v1\0";

/// Checks the signature against any of `keys` and the expiry, like agentupdate.Verify.
pub fn verify(
    manifest_b64: &str,
    signature_b64: &str,
    keys: &[[u8; 32]],
    now: time::OffsetDateTime,
) -> Result<Manifest, String> {
    let bytes = B64
        .decode(manifest_b64)
        .map_err(|_| "bad manifest encoding")?;
    let sig = B64
        .decode(signature_b64)
        .map_err(|_| "bad signature encoding")?;
    let sig = Signature::from_slice(&sig).map_err(|_| "bad signature")?;
    let mut msg = SIGN_CONTEXT.to_vec();
    msg.extend_from_slice(&bytes);
    let ok = keys
        .iter()
        .filter_map(|k| VerifyingKey::from_bytes(k).ok())
        .any(|k| k.verify(&msg, &sig).is_ok());
    if !ok {
        return Err("the update is not signed by this server".into());
    }
    let m: Manifest = serde_json::from_slice(&bytes).map_err(|e| e.to_string())?;
    let exp = time::OffsetDateTime::parse(
        &m.expires_at,
        &time::format_description::well_known::Rfc3339,
    )
    .map_err(|_| "bad expiry in the update")?;
    if exp < now {
        return Err("the update offer has expired".into());
    }
    Ok(m)
}

pub fn check(cfg: &Config, ctx: Option<&UpdateCtx>) -> Result<Option<Offer>, String> {
    if !cfg.update_keys.is_empty() {
        return check_signed(cfg, ctx.ok_or("not connected to the server yet")?);
    }
    let server = ctx.map_or(cfg.server.as_str(), |c| c.server.as_str());
    #[derive(Deserialize)]
    struct Latest {
        version: String,
        url: String,
        sha256: String,
    }
    let l: Latest = http()
        .get(format!("{server}/api/agent/v1/quick/latest"))
        .call()
        .map_err(|e| e.to_string())?
        .body_mut()
        .read_json()
        .map_err(|e| e.to_string())?;
    if !is_newer(&l.version, VERSION) {
        return Ok(None);
    }
    Ok(Some(Offer {
        version: l.version,
        url: absolute(server, &l.url),
        sha256: l.sha256,
        size: None,
        bearer: None,
    }))
}

fn check_signed(cfg: &Config, ctx: &UpdateCtx) -> Result<Option<Offer>, String> {
    let url = format!(
        "{}/api/agent/v1/update?product=quick&os=windows&arch={}&version=v{VERSION}&channel={}",
        ctx.server,
        arch(),
        cfg.channel
    );
    let mut resp = http()
        .get(&url)
        .header("Authorization", format!("Bearer {}", ctx.token))
        .call()
        .map_err(|e| e.to_string())?;
    if resp.status().as_u16() == 204 {
        return Ok(None);
    }
    let s: Signed = resp.body_mut().read_json().map_err(|e| e.to_string())?;
    let m = verify(
        &s.manifest,
        &s.signature,
        &cfg.update_keys,
        time::OffsetDateTime::now_utc(),
    )?;
    if m.product != "rhis-quick" || m.device_id != ctx.device_id {
        return Err("the update is not meant for this program".into());
    }
    if !is_newer(&m.version, VERSION) && !m.allow_downgrade {
        return Ok(None);
    }
    Ok(Some(Offer {
        version: m.version,
        url: absolute(&ctx.server, &m.url),
        sha256: m.sha256,
        size: Some(m.size),
        bearer: Some(ctx.token.clone()),
    }))
}

fn arch() -> &'static str {
    if cfg!(target_arch = "aarch64") {
        "arm64"
    } else {
        "amd64"
    }
}

fn absolute(server: &str, url: &str) -> String {
    if url.starts_with('/') {
        format!("{server}{url}")
    } else {
        url.to_string()
    }
}

/// Downloads the update to the temp folder, checks size and SHA-256, keeps this
/// copy's configuration (servers, branding, keys) and starts it. The caller then
/// closes this window; the new copy registers a new code.
pub fn download_and_start(cfg: &Config, offer: &Offer) -> Result<(), String> {
    use sha2::{Digest, Sha256};
    use std::io::Read;
    validate_server(&offer.url, cfg.insecure_dev).map_err(|_| "the update address is not https")?;
    let mut req = http().get(&offer.url);
    if let Some(t) = &offer.bearer {
        req = req.header("Authorization", format!("Bearer {t}"));
    }
    let limit = offer
        .size
        .unwrap_or(200 * 1024 * 1024)
        .min(200 * 1024 * 1024);
    let mut body = Vec::new();
    req.call()
        .map_err(|e| e.to_string())?
        .body_mut()
        .as_reader()
        .take(limit + 1)
        .read_to_end(&mut body)
        .map_err(|e| e.to_string())?;
    if offer.size.is_some_and(|n| body.len() as u64 != n) {
        return Err("the download has the wrong size".into());
    }
    if !hex::encode(Sha256::digest(&body)).eq_ignore_ascii_case(offer.sha256.trim()) {
        return Err("checksum mismatch".into());
    }
    // Update files come without configuration: carry over our own.
    if trailer_bytes(&body).is_none()
        && let Some(own) = std::env::current_exe()
            .ok()
            .and_then(|p| std::fs::read(p).ok())
        && let Some(t) = trailer_bytes(&own)
    {
        body.extend_from_slice(t);
    }
    let path = std::env::temp_dir().join(format!(
        "rhis-quicksupport-{}.exe",
        offer.version.trim_start_matches('v')
    ));
    std::fs::write(&path, &body).map_err(|e| e.to_string())?;
    std::process::Command::new(&path)
        .spawn()
        .map_err(|e| e.to_string())?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signer, SigningKey};

    fn signed(key: &SigningKey, manifest: &str) -> (String, String) {
        let mut msg = SIGN_CONTEXT.to_vec();
        msg.extend_from_slice(manifest.as_bytes());
        (B64.encode(manifest), B64.encode(key.sign(&msg).to_bytes()))
    }

    #[test]
    fn verifies_like_the_server() {
        let key = SigningKey::from_bytes(&[7; 32]);
        let other = SigningKey::from_bytes(&[8; 32]);
        let keys = [key.verifying_key().to_bytes()];
        let now = time::macros::datetime!(2026-11-03 10:00 UTC);
        let m = r#"{"product":"rhis-quick","version":"v0.3.0","os":"windows","arch":"amd64","size":5,"sha256":"ab","url":"/x","expires_at":"2026-11-04T10:00:00Z","device_id":12,"allow_downgrade":false}"#;
        let (mb, sb) = signed(&key, m);
        let got = verify(&mb, &sb, &keys, now).unwrap();
        assert_eq!((got.version.as_str(), got.device_id), ("v0.3.0", 12));
        // Wrong key, tampered manifest, expired offer.
        let (mb2, sb2) = signed(&other, m);
        assert!(verify(&mb2, &sb2, &keys, now).is_err());
        assert!(verify(&B64.encode(m.replace("v0.3.0", "v9.9.9")), &sb, &keys, now).is_err());
        let later = time::macros::datetime!(2026-11-05 10:00 UTC);
        assert!(verify(&mb, &sb, &keys, later).is_err());
    }
}
