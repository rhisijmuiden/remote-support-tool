//! The Windows service: keeps the control channel open and handles Ctrl+Alt+Del requests.
use std::ffi::OsString;
use std::path::PathBuf;
use std::sync::{Arc, OnceLock};
use std::time::Duration;

use anyhow::{Context, Result};
use tracing::{error, info};
use windows::Win32::Foundation::{CloseHandle, WAIT_OBJECT_0};
use windows::Win32::System::Threading::{CreateEventW, WaitForSingleObject};
use windows::core::w;
use windows_service::service::{
    ServiceControl, ServiceControlAccept, ServiceExitCode, ServiceState, ServiceStatus, ServiceType,
};
use windows_service::service_control_handler::{self, ServiceControlHandlerResult};
use windows_service::{define_windows_service, service_dispatcher};

use crate::agent::{Agent, Launcher};
use crate::config::AgentConfig;

pub const SERVICE_NAME: &str = "RHISRemoteAgent";

static CONFIG_PATH: OnceLock<PathBuf> = OnceLock::new();

define_windows_service!(ffi_service_main, service_main);

pub fn run(path: PathBuf) -> Result<()> {
    let _ = CONFIG_PATH.set(path);
    service_dispatcher::start(SERVICE_NAME, ffi_service_main)
        .context("start the service dispatcher (run this from the service manager)")?;
    Ok(())
}

fn service_main(_args: Vec<OsString>) {
    if let Err(e) = run_service() {
        error!("service stopped: {e:#}");
    }
}

fn run_service() -> Result<()> {
    let (stop_tx, mut stop_rx) = tokio::sync::watch::channel(false);
    let handler = move |c| match c {
        ServiceControl::Stop | ServiceControl::Shutdown => {
            let _ = stop_tx.send(true);
            ServiceControlHandlerResult::NoError
        }
        ServiceControl::Interrogate => ServiceControlHandlerResult::NoError,
        _ => ServiceControlHandlerResult::NotImplemented,
    };
    let status = service_control_handler::register(SERVICE_NAME, handler)?;
    let set = |state, exit| {
        let _ = status.set_service_status(ServiceStatus {
            service_type: ServiceType::OWN_PROCESS,
            current_state: state,
            controls_accepted: if state == ServiceState::Running {
                ServiceControlAccept::STOP | ServiceControlAccept::SHUTDOWN
            } else {
                ServiceControlAccept::empty()
            },
            exit_code: ServiceExitCode::Win32(exit),
            checkpoint: 0,
            wait_hint: Duration::from_secs(5),
            process_id: None,
        });
    };
    set(ServiceState::Running, 0);
    info!("service started, version {}", rhis_core::VERSION);
    std::thread::spawn(sas_watcher);

    let path = CONFIG_PATH
        .get()
        .cloned()
        .unwrap_or_else(crate::config::default_path);
    let result = crate::agent::runtime().block_on(async move {
        let cfg = AgentConfig::load(&path)?.context("not enrolled: run rhis-agent install")?;
        // Before anything that can fail: tell the update helper this version starts.
        crate::update::mark_started(&path);
        let token = cfg.token()?;
        let wake = Arc::new(tokio::sync::Notify::new());
        let restart = Arc::new(tokio::sync::Notify::new());
        let agent = Arc::new(Agent {
            cfg: cfg.clone(),
            launcher: Launcher::Helper,
            token: token.clone(),
            active: Default::default(),
            update_wake: wake.clone(),
        });
        let cfg_path = path.clone();
        let updater = Arc::new(crate::update::Updater {
            cfg,
            cfg_path: path.clone(),
            token,
            wake,
            active: agent.active.clone(),
            restart: restart.clone(),
            apply: Box::new(move |bytes, p| crate::win::update::apply(&cfg_path, bytes, p)),
        });
        tokio::spawn(updater.run());
        tokio::select! {
            r = crate::agent::run_control(agent) => r,
            _ = stop_rx.wait_for(|s| *s) => Ok(()),
            _ = restart.notified() => {
                info!("stopping for the update; the update helper starts the new version");
                Ok(())
            }
        }
    });
    if let Err(e) = &result {
        error!("{e:#}");
    }
    set(ServiceState::Stopped, if result.is_ok() { 0 } else { 1 });
    info!("service stopped");
    Ok(())
}

/// Session helpers set this event to ask for Ctrl+Alt+Del; SendSAS works from a service.
fn sas_watcher() {
    unsafe {
        let Ok(ev) = CreateEventW(None, false, false, w!(r"Global\RHISRemoteSAS")) else {
            error!("cannot create the Ctrl+Alt+Del event");
            return;
        };
        loop {
            if WaitForSingleObject(ev, u32::MAX) == WAIT_OBJECT_0 {
                info!("sending Ctrl+Alt+Del");
                rhis_core::platform::windows::call_send_sas();
            } else {
                break;
            }
        }
        let _ = CloseHandle(ev);
    }
}
