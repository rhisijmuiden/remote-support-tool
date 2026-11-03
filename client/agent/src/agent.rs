//! The control loop and session handling shared by the service, the foreground mode and the helper.
use std::path::Path;
use std::sync::Arc;

use anyhow::{Context, Result};
use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD as B64;
use rhis_core::control::{ControlConfig, ControlError, ControlHandler, Decision};
use rhis_core::net::ServerConfig;
use rhis_core::protocol::SessionRequest;
use rhis_core::session::{self, SessionConfig};
use tracing::{error, info, warn};

use crate::config::AgentConfig;
use crate::sysinfo;

pub fn runtime() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("tokio runtime")
}

/// Logs to stderr, and for the service and helper also to agent.log next to agent.json.
pub fn init_logging(cfg_path: &Path, to_file: bool) {
    use tracing_subscriber::EnvFilter;
    let filter = EnvFilter::try_from_env("RHIS_LOG")
        .unwrap_or_else(|_| EnvFilter::new("info,webrtc=warn,rtc=warn"));
    let file = if to_file {
        cfg_path.parent().and_then(|d| {
            std::fs::create_dir_all(d).ok()?;
            let p = d.join("agent.log");
            // Keep the log small: start over above 5 MB.
            if std::fs::metadata(&p)
                .map(|m| m.len() > 5 << 20)
                .unwrap_or(false)
            {
                let _ = std::fs::rename(&p, d.join("agent.log.1"));
            }
            std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(p)
                .ok()
        })
    } else {
        None
    };
    let b = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_target(false);
    match file {
        Some(f) => b
            .with_ansi(false)
            .with_writer(std::sync::Mutex::new(f))
            .init(),
        None => b.with_writer(std::io::stderr).init(),
    }
}

pub fn servers(cfg: &AgentConfig) -> Result<Vec<ServerConfig>> {
    let pin = Some(cfg.server_cert_sha256.as_str()).filter(|p| !p.is_empty());
    let list: Vec<ServerConfig> = cfg
        .servers
        .iter()
        .filter_map(|s| {
            ServerConfig::new(s, pin, cfg.insecure_dev)
                .map_err(|e| warn!("{s}: {e}"))
                .ok()
        })
        .collect();
    anyhow::ensure!(
        !list.is_empty(),
        "no usable server URL in the configuration"
    );
    Ok(list)
}

pub fn control_config(cfg: &AgentConfig) -> Result<ControlConfig> {
    let mut list = servers(cfg)?;
    let info = sysinfo::collect();
    Ok(ControlConfig {
        server: list.remove(0),
        alternates: list,
        token: cfg.token()?,
        machine_id: cfg.machine_id.clone(),
        hostname: info.hostname,
        os_version: info.os_version,
        agent_version: rhis_core::VERSION.to_string(),
    })
}

/// How a session is started once accepted.
pub enum Launcher {
    /// In this process (foreground mode, development).
    InProcess,
    /// In a helper process in the console session (Windows service).
    #[cfg(windows)]
    Helper,
}

pub struct Agent {
    pub cfg: AgentConfig,
    pub launcher: Launcher,
    pub token: String,
    pub active: Arc<std::sync::Mutex<std::collections::HashSet<String>>>,
}

#[async_trait::async_trait]
impl ControlHandler for Agent {
    async fn on_welcome(&self, device_id: i64, code: &str) {
        info!(
            "online: device {device_id}, ID {}",
            crate::format_code(code)
        );
    }

    async fn on_session_request(&self, req: SessionRequest, server: ServerConfig) -> Decision {
        // Unattended access: sessions from signed-in technicians are accepted. The session
        // banner on screen tells a local user that someone is connected.
        if self.active.lock().unwrap().len() >= 4 {
            return Decision::Reject("busy".into());
        }
        info!(
            "session {} requested by {} ({}, recording: {})",
            req.session_id, req.user_name, req.kind, req.recording
        );
        let id = req.session_id.clone();
        self.active.lock().unwrap().insert(id.clone());
        let active = self.active.clone();
        let started: Result<()> = match self.launcher {
            Launcher::InProcess => {
                let sc = SessionConfig {
                    server,
                    token: self.token.clone(),
                    request: req,
                    fps: 30.0,
                    product_name: self.cfg.product_name(),
                    cancel: None,
                };
                let id = id.clone();
                tokio::spawn(async move {
                    match session::run(sc, Arc::from(rhis_core::platform::default_platform())).await
                    {
                        Ok(r) => info!("session ended: {r}"),
                        Err(e) => warn!("session failed: {e:#}"),
                    }
                    active.lock().unwrap().remove(&id);
                });
                Ok(())
            }
            #[cfg(windows)]
            Launcher::Helper => crate::win::spawn::start_helper(&req, &server).map(|mut child| {
                let id = id.clone();
                std::thread::spawn(move || {
                    let _ = child.wait();
                    active.lock().unwrap().remove(&id);
                });
            }),
        };
        match started {
            Ok(()) => Decision::Accept,
            Err(e) => {
                error!("cannot start session: {e:#}");
                self.active.lock().unwrap().remove(&id);
                Decision::Reject("error".into())
            }
        }
    }

    async fn on_message(&self, msg: serde_json::Value) {
        info!("server message: {}", msg["type"]);
    }

    fn console_user(&self) -> String {
        sysinfo::console_user()
    }
}

/// Runs the control channel until the device token is refused.
pub async fn run_control(agent: Arc<Agent>) -> Result<()> {
    let cc = control_config(&agent.cfg)?;
    loop {
        match rhis_core::control::run(cc.clone(), agent.clone()).await {
            ControlError::Unauthorized => {
                // The device was deleted in the portal or enrolled again. Retry slowly in case it is restored.
                error!("the server refused this device; enroll it again with `rhis-agent install`");
                tokio::time::sleep(std::time::Duration::from_secs(300)).await;
            }
            ControlError::Other(e) => return Err(e),
        }
    }
}

pub async fn run_foreground(cfg: AgentConfig, _path: std::path::PathBuf) -> Result<()> {
    let token = cfg.token()?;
    let agent = Arc::new(Agent {
        cfg,
        launcher: Launcher::InProcess,
        token,
        active: Default::default(),
    });
    tokio::select! {
        r = run_control(agent) => r,
        _ = tokio::signal::ctrl_c() => Ok(()),
    }
}

/// Encodes a session request for the helper's command line.
#[cfg_attr(not(windows), allow(dead_code))]
pub fn encode_request(req: &SessionRequest) -> String {
    B64.encode(serde_json::to_vec(req).unwrap())
}

/// The session helper: runs one session in this process and exits.
pub fn run_helper(path: &Path, request: &str, server: &str) -> Result<()> {
    let cfg = AgentConfig::load(path)?.context("not enrolled")?;
    let req: SessionRequest = serde_json::from_slice(&B64.decode(request)?)?;
    let server = servers(&cfg)?
        .into_iter()
        .find(|s| s.url.as_str().trim_end_matches('/') == server.trim_end_matches('/'))
        .context("unknown server")?;
    #[cfg(windows)]
    crate::win::sys::set_dpi_aware();
    let sc = SessionConfig {
        server,
        token: cfg.token()?,
        request: req,
        fps: 30.0,
        product_name: cfg.product_name(),
        cancel: None,
    };
    let reason = runtime().block_on(session::run(
        sc,
        Arc::from(rhis_core::platform::default_platform()),
    ))?;
    info!("session helper done: {reason}");
    Ok(())
}
