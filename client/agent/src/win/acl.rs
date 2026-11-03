//! Limits the configuration folder to SYSTEM and Administrators.
use std::os::windows::process::CommandExt;
use std::path::Path;
use std::process::Command;

use anyhow::{Result, bail};

const CREATE_NO_WINDOW: u32 = 0x0800_0000;

pub fn restrict_to_system_and_admins(dir: &Path) -> Result<()> {
    // S-1-5-18 = SYSTEM, S-1-5-32-544 = Administrators (language independent).
    let st = Command::new("icacls")
        .arg(dir)
        .args([
            "/inheritance:r",
            "/grant:r",
            "*S-1-5-18:(OI)(CI)F",
            "*S-1-5-32-544:(OI)(CI)F",
            "/q",
        ])
        .creation_flags(CREATE_NO_WINDOW)
        .status()?;
    if !st.success() {
        bail!("icacls failed on {}", dir.display());
    }
    Ok(())
}
