//! Protects the device token at rest.
//!
//! On Windows the token is encrypted with DPAPI in machine scope and the configuration folder
//! is limited to SYSTEM and Administrators. Elsewhere (development builds) the file is mode 0600.
use std::path::Path;

use anyhow::Result;
use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;

#[cfg(windows)]
pub fn protect(plain: &str) -> Result<String> {
    Ok(format!(
        "dpapi:{}",
        B64.encode(crate::win::dpapi::protect(plain.as_bytes())?)
    ))
}

#[cfg(windows)]
pub fn unprotect(stored: &str) -> Result<String> {
    match stored.strip_prefix("dpapi:") {
        Some(b) => Ok(String::from_utf8(crate::win::dpapi::unprotect(
            &B64.decode(b)?,
        )?)?),
        None => Ok(stored.to_string()),
    }
}

#[cfg(not(windows))]
pub fn protect(plain: &str) -> Result<String> {
    Ok(format!("plain:{}", B64.encode(plain)))
}

#[cfg(not(windows))]
pub fn unprotect(stored: &str) -> Result<String> {
    match stored.strip_prefix("plain:") {
        Some(b) => Ok(String::from_utf8(B64.decode(b)?)?),
        None => Ok(stored.to_string()),
    }
}

#[cfg(windows)]
pub fn restrict_dir(dir: &Path) -> Result<()> {
    crate::win::acl::restrict_to_system_and_admins(dir)
}

#[cfg(not(windows))]
pub fn restrict_dir(dir: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
    Ok(())
}

#[cfg(windows)]
pub fn restrict_file(_f: &Path) -> Result<()> {
    Ok(()) // inherits the folder's ACL
}

#[cfg(not(windows))]
pub fn restrict_file(f: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(f, std::fs::Permissions::from_mode(0o600))?;
    Ok(())
}
