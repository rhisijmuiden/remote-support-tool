//! Installs the agent as an automatic Windows service that restarts on failure.
use std::ffi::OsString;
use std::os::windows::process::CommandExt;
use std::path::PathBuf;
use std::process::Command;
use std::time::Duration;

use anyhow::{Context, Result, bail};
use windows::Win32::UI::Shell::IsUserAnAdmin;
use windows_service::service::{
    ServiceAccess, ServiceAction, ServiceActionType, ServiceErrorControl, ServiceFailureActions,
    ServiceFailureResetPeriod, ServiceInfo, ServiceStartType, ServiceState, ServiceType,
};
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};
use winreg::RegKey;
use winreg::enums::{HKEY_LOCAL_MACHINE, KEY_WOW64_64KEY, KEY_WRITE};

use super::service::SERVICE_NAME;

const CREATE_NO_WINDOW: u32 = 0x0800_0000;

pub fn require_admin() -> Result<()> {
    if !unsafe { IsUserAnAdmin() }.as_bool() {
        bail!("run this from an administrator command prompt or PowerShell");
    }
    Ok(())
}

pub fn install_dir() -> PathBuf {
    let base = std::env::var_os("ProgramFiles")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(r"C:\Program Files"));
    base.join("RHIS Remote")
}

fn hidden(cmd: &mut Command) -> &mut Command {
    cmd.creation_flags(CREATE_NO_WINDOW)
}

pub(crate) fn stop_and_wait(svc: &windows_service::service::Service) {
    if let Ok(st) = svc.query_status()
        && st.current_state != ServiceState::Stopped
    {
        let _ = svc.stop();
        for _ in 0..40 {
            std::thread::sleep(Duration::from_millis(250));
            if svc
                .query_status()
                .map(|s| s.current_state == ServiceState::Stopped)
                .unwrap_or(true)
            {
                break;
            }
        }
    }
}

pub fn install_service() -> Result<()> {
    let dir = install_dir();
    std::fs::create_dir_all(&dir)?;
    let target = dir.join("rhis-agent.exe");
    let mgr = ServiceManager::local_computer(
        None::<&str>,
        ServiceManagerAccess::CONNECT | ServiceManagerAccess::CREATE_SERVICE,
    )
    .context("open the service manager")?;
    let access = ServiceAccess::QUERY_STATUS
        | ServiceAccess::START
        | ServiceAccess::STOP
        | ServiceAccess::CHANGE_CONFIG;
    let existing = mgr.open_service(SERVICE_NAME, access).ok();
    if let Some(svc) = &existing {
        stop_and_wait(svc);
    }
    let me = std::env::current_exe()?;
    if me.canonicalize().ok() != target.canonicalize().ok() {
        std::fs::copy(&me, &target)
            .with_context(|| format!("copy the agent to {}", target.display()))?;
    }
    let info = ServiceInfo {
        name: OsString::from(SERVICE_NAME),
        display_name: OsString::from("RHIS Remote Agent"),
        service_type: ServiceType::OWN_PROCESS,
        start_type: ServiceStartType::AutoStart,
        error_control: ServiceErrorControl::Normal,
        executable_path: target.clone(),
        launch_arguments: vec![OsString::from("service")],
        dependencies: vec![],
        account_name: None, // LocalSystem
        account_password: None,
    };
    let svc = match existing {
        Some(svc) => {
            svc.change_config(&info)?;
            svc
        }
        None => mgr.create_service(&info, access)?,
    };
    let _ = svc.set_description(
        "Keeps this computer reachable for remote support (RHIS Remote). Outbound HTTPS only.",
    );
    // Restart after 5 s, 30 s and then every 60 s; the failure count resets after a day.
    let restart = |s| ServiceAction {
        action_type: ServiceActionType::Restart,
        delay: Duration::from_secs(s),
    };
    let _ = svc.update_failure_actions(ServiceFailureActions {
        reset_period: ServiceFailureResetPeriod::After(Duration::from_secs(86400)),
        reboot_msg: None,
        command: None,
        actions: Some(vec![restart(5), restart(30), restart(60)]),
    });
    let _ = svc.set_failure_actions_on_non_crash_failures(true);

    // Allow services to send Ctrl+Alt+Del (SoftwareSASGeneration = 1).
    if let Ok((k, _)) = RegKey::predef(HKEY_LOCAL_MACHINE).create_subkey_with_flags(
        r"SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System",
        KEY_WRITE | KEY_WOW64_64KEY,
    ) {
        let _ = k.set_value("SoftwareSASGeneration", &1u32);
    }
    // Let WebRTC accept direct connections on the local network.
    let _ = hidden(Command::new("netsh").args([
        "advfirewall",
        "firewall",
        "delete",
        "rule",
        "name=RHIS Remote Agent",
    ]))
    .status();
    let _ = hidden(Command::new("netsh").args([
        "advfirewall",
        "firewall",
        "add",
        "rule",
        "name=RHIS Remote Agent",
        "dir=in",
        "action=allow",
        "protocol=UDP",
        &format!("program={}", target.display()),
        "enable=yes",
    ]))
    .status();
    svc.start::<&str>(&[]).context("start the service")?;
    Ok(())
}

pub fn uninstall_service() -> Result<()> {
    let mgr = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)?;
    if let Ok(svc) = mgr.open_service(
        SERVICE_NAME,
        ServiceAccess::QUERY_STATUS | ServiceAccess::STOP | ServiceAccess::DELETE,
    ) {
        stop_and_wait(&svc);
        svc.delete()?;
    }
    let _ = hidden(Command::new("netsh").args([
        "advfirewall",
        "firewall",
        "delete",
        "rule",
        "name=RHIS Remote Agent",
    ]))
    .status();
    // The running exe cannot delete itself; remove the folder a moment after we exit.
    let dir = install_dir();
    if dir.exists() {
        let _ = hidden(Command::new("cmd").args([
            "/c",
            &format!(
                "ping -n 3 127.0.0.1 >nul & rmdir /s /q \"{}\"",
                dir.display()
            ),
        ]))
        .spawn();
    }
    Ok(())
}
