//! Installing an update on Windows.
//!
//! The running service renames its own executable to `rhis-agent.old.exe` (Windows allows
//! renaming a running file), writes the verified update in its place and starts
//! `rhis-agent finish-update` from the new file. The service then stops. The helper waits
//! for that, starts the service again and waits for the new version to say it started
//! (`pending.json` state "started"). When it does not within 90 seconds, the helper puts
//! the old executable back and starts that one, which reports the failure.
use std::os::windows::process::CommandExt;
use std::path::Path;
use std::process::Command;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use tracing::{error, info, warn};
use windows_service::service::{ServiceAccess, ServiceState};
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};

use super::service::SERVICE_NAME;
use crate::update::{Pending, read_pending, write_pending};

const CREATE_NO_WINDOW: u32 = 0x0800_0000;
const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
const CREATE_BREAKAWAY_FROM_JOB: u32 = 0x0100_0000;

/// How long the new version gets to start before it is rolled back.
const START_TIMEOUT: Duration = Duration::from_secs(90);

/// Swaps the executable and starts the helper. Called by the service.
pub fn apply(cfg_path: &Path, bytes: &[u8], p: &Pending) -> Result<Pending> {
    let target = std::env::current_exe()?;
    let backup = target.with_file_name("rhis-agent.old.exe");
    let _ = std::fs::remove_file(&backup);
    std::fs::rename(&target, &backup).context("move the running agent aside")?;
    let restore = || {
        let _ = std::fs::remove_file(&target);
        let _ = std::fs::rename(&backup, &target);
    };
    if let Err(e) = std::fs::write(&target, bytes) {
        restore();
        return Err(e).context("write the new agent");
    }
    let mut p = p.clone();
    p.backup = backup.clone();
    if let Err(e) = write_pending(cfg_path, &p) {
        restore();
        return Err(e);
    }
    let spawn = |flags| {
        Command::new(&target)
            .arg("--config")
            .arg(cfg_path)
            .arg("finish-update")
            .creation_flags(flags)
            .spawn()
    };
    let base = CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP;
    // Out of the service's job, if it has one, so stopping the service does not end the helper.
    if let Err(e) = spawn(base | CREATE_BREAKAWAY_FROM_JOB).or_else(|_| spawn(base)) {
        restore();
        let _ = std::fs::remove_file(crate::update::updates_dir(cfg_path).join("pending.json"));
        return Err(e).context("start the update helper");
    }
    Ok(p)
}

fn wait_for(svc: &windows_service::service::Service, state: ServiceState, max: Duration) -> bool {
    let start = Instant::now();
    while start.elapsed() < max {
        if svc.query_status().is_ok_and(|s| s.current_state == state) {
            return true;
        }
        std::thread::sleep(Duration::from_millis(250));
    }
    false
}

/// `rhis-agent finish-update`: runs from the new executable while the service restarts.
pub fn finish(cfg_path: &Path) -> Result<()> {
    let p = read_pending(cfg_path).context("no update in progress")?;
    let mgr = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)?;
    let svc = mgr.open_service(
        SERVICE_NAME,
        ServiceAccess::QUERY_STATUS | ServiceAccess::START | ServiceAccess::STOP,
    )?;
    if !wait_for(&svc, ServiceState::Stopped, Duration::from_secs(60)) {
        return rollback(cfg_path, &svc, p, "the service did not stop for the update");
    }
    info!("starting version {}", p.to_version);
    if let Err(e) = svc.start::<&str>(&[]) {
        return rollback(
            cfg_path,
            &svc,
            p,
            &format!("the new version did not start: {e}"),
        );
    }
    let start = Instant::now();
    while start.elapsed() < START_TIMEOUT {
        match read_pending(cfg_path) {
            // Started, or already reported and cleaned up.
            Some(q) if q.state == "started" => return Ok(()),
            None => return Ok(()),
            _ => std::thread::sleep(Duration::from_secs(1)),
        }
    }
    rollback(
        cfg_path,
        &svc,
        p,
        "the new version did not start within 90 seconds",
    )
}

fn rollback(
    cfg_path: &Path,
    svc: &windows_service::service::Service,
    mut p: Pending,
    why: &str,
) -> Result<()> {
    error!(
        "update to {} failed: {why}; going back to {}",
        p.to_version, p.from_version
    );
    super::install::stop_and_wait(svc);
    let target = std::env::current_exe()?;
    let failed = target.with_file_name("rhis-agent.failed.exe");
    let _ = std::fs::remove_file(&failed);
    // Renaming works even when the failed version still runs.
    std::fs::rename(&target, &failed).context("move the failed version aside")?;
    if let Err(e) = std::fs::rename(&p.backup, &target) {
        let _ = std::fs::rename(&failed, &target);
        return Err(e).context("put the old version back");
    }
    p.state = "rolled_back".into();
    p.error = why.to_string();
    write_pending(cfg_path, &p)?;
    if let Err(e) = svc.start::<&str>(&[]) {
        warn!("start the old version: {e}");
    }
    Ok(())
}
