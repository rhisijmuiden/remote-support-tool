//! The control channel: keeps the device online and receives session requests.
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Result, anyhow};
use futures_util::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::{self, Message};
use tracing::{info, warn};

use crate::net::ServerConfig;
use crate::protocol::{ClientMsg, ServerMsg, SessionRequest};

/// Identity and connection settings of this client.
#[derive(Clone)]
pub struct ControlConfig {
    pub server: ServerConfig,
    /// Further servers to try, in order, when `server` cannot be reached.
    pub alternates: Vec<ServerConfig>,
    pub token: String,
    pub machine_id: String,
    pub hostname: String,
    pub os_version: String,
    pub agent_version: String,
}

/// What to do with a session request.
#[derive(Debug, Clone, PartialEq)]
pub enum Decision {
    Accept,
    Reject(String),
}

#[async_trait::async_trait]
pub trait ControlHandler: Send + Sync + 'static {
    async fn on_welcome(&self, _device_id: i64, _device_code: &str) {}
    /// Decide on a session request. For an attended or quick session, ask the local user here.
    /// After `Accept`, start the session (for example with [`crate::session::run`]).
    /// `server` is the server this control channel is connected to; open the session socket there.
    async fn on_session_request(&self, req: SessionRequest, server: ServerConfig) -> Decision;
    async fn on_session_cancel(&self, _session_id: &str) {}
    /// Messages this crate does not handle itself, such as `update_available`.
    async fn on_message(&self, _msg: serde_json::Value) {}
    /// The user signed in at the console, reported to the portal ("" when nobody is signed in).
    fn console_user(&self) -> String {
        String::new()
    }
}

#[derive(Debug)]
pub enum ControlError {
    /// The server refused the device token (device deleted or re-enrolled elsewhere).
    Unauthorized,
    Other(anyhow::Error),
}

impl std::fmt::Display for ControlError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ControlError::Unauthorized => write!(f, "the server refused this device's token"),
            ControlError::Other(e) => write!(f, "{e:#}"),
        }
    }
}

/// Keeps the control channel open, reconnecting with backoff (1 s up to 60 s, with jitter).
/// Returns only when the server refuses the token.
pub async fn run(cfg: ControlConfig, handler: Arc<dyn ControlHandler>) -> ControlError {
    let mut backoff = Duration::from_secs(1);
    let servers: Vec<ServerConfig> = std::iter::once(cfg.server.clone())
        .chain(cfg.alternates.iter().cloned())
        .collect();
    let mut idx = 0;
    loop {
        let started = std::time::Instant::now();
        let server = &servers[idx % servers.len()];
        match connect_once(&cfg, server, handler.clone()).await {
            Err(ControlError::Unauthorized) => return ControlError::Unauthorized,
            Err(e) => {
                warn!("control channel to {}: {e}", server.url);
                idx += 1; // fail over to the next server
            }
            Ok(()) => info!("control channel closed"),
        }
        if started.elapsed() > Duration::from_secs(60) {
            backoff = Duration::from_secs(1);
        }
        let jitter = Duration::from_millis(rand::random::<u64>() % 1000);
        tokio::time::sleep(backoff + jitter).await;
        backoff = (backoff * 2).min(Duration::from_secs(60));
    }
}

fn is_unauthorized(e: &anyhow::Error) -> bool {
    matches!(e.downcast_ref::<tungstenite::Error>(), Some(tungstenite::Error::Http(r)) if r.status() == 401)
}

async fn connect_once(
    cfg: &ControlConfig,
    server: &ServerConfig,
    handler: Arc<dyn ControlHandler>,
) -> Result<(), ControlError> {
    let ws = server
        .connect_ws("/api/agent/v1/control", &cfg.token)
        .await
        .map_err(|e| {
            if is_unauthorized(&e) {
                ControlError::Unauthorized
            } else {
                ControlError::Other(e)
            }
        })?;
    let (mut sink, mut stream) = ws.split();
    let (tx, mut rx) = mpsc::channel::<ClientMsg>(32);
    let mut user = handler.console_user();
    let hello = ClientMsg::Hello {
        hostname: cfg.hostname.clone(),
        os_version: cfg.os_version.clone(),
        agent_version: cfg.agent_version.clone(),
        user: user.clone(),
        machine_id: cfg.machine_id.clone(),
    };
    send(&mut sink, &hello).await.map_err(ControlError::Other)?;
    let mut ping_interval = Duration::from_secs(25);
    let mut last_rx = std::time::Instant::now();
    let mut tick = tokio::time::interval(Duration::from_secs(5));
    loop {
        tokio::select! {
            m = stream.next() => {
                let text = match m {
                    Some(Ok(Message::Text(t))) => t,
                    Some(Ok(Message::Close(_))) | None => return Ok(()),
                    Some(Ok(_)) => { last_rx = std::time::Instant::now(); continue }
                    Some(Err(e)) => return Err(ControlError::Other(e.into())),
                };
                last_rx = std::time::Instant::now();
                match ServerMsg::parse(&text) {
                    Ok(ServerMsg::Welcome { device_id, device_code, ping_interval: p }) => {
                        ping_interval = Duration::from_secs(p.max(5));
                        info!("online as device {device_id} ({device_code})");
                        handler.on_welcome(device_id, &device_code).await;
                    }
                    Ok(ServerMsg::Ping) => send(&mut sink, &ClientMsg::Pong).await.map_err(ControlError::Other)?,
                    Ok(ServerMsg::SessionRequest(req)) => {
                        let (h, tx, srv) = (handler.clone(), tx.clone(), server.clone());
                        // Decide in a task, so a prompt for the local user does not block pings.
                        tokio::spawn(async move {
                            let id = req.session_id.clone();
                            let msg = match h.on_session_request(req, srv).await {
                                Decision::Accept => ClientMsg::SessionAccept { session_id: id },
                                Decision::Reject(reason) => ClientMsg::SessionReject { session_id: id, reason },
                            };
                            let _ = tx.send(msg).await;
                        });
                    }
                    Ok(ServerMsg::SessionCancel { session_id }) => handler.on_session_cancel(&session_id).await,
                    Ok(ServerMsg::Other(v)) => handler.on_message(v).await,
                    Err(e) => warn!("bad control message: {e}"),
                }
            }
            Some(m) = rx.recv() => send(&mut sink, &m).await.map_err(ControlError::Other)?,
            _ = tick.tick() => {
                let u = handler.console_user();
                if u != user {
                    user = u.clone();
                    send(&mut sink, &ClientMsg::Status { user: u }).await.map_err(ControlError::Other)?;
                }
                // The server pings every ping_interval; silence for three intervals means the link is dead.
                if last_rx.elapsed() > ping_interval * 3 {
                    return Err(ControlError::Other(anyhow!("server stopped responding")));
                }
            }
        }
    }
}

async fn send<S>(sink: &mut S, m: &ClientMsg) -> Result<()>
where
    S: SinkExt<Message> + Unpin,
    S::Error: std::error::Error + Send + Sync + 'static,
{
    let text = serde_json::to_string(m)?;
    sink.send(Message::Text(text.into())).await?;
    Ok(())
}

/// Request and response of `POST /api/agent/v1/enroll`.
#[derive(Debug, Serialize)]
pub struct EnrollRequest<'a> {
    pub enroll_token: &'a str,
    pub machine_id: &'a str,
    pub hostname: &'a str,
    pub os_version: &'a str,
    pub agent_version: &'a str,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Enrollment {
    pub device_id: i64,
    pub device_code: String,
    pub device_token: String,
    /// Public keys (base64 Ed25519) that sign updates, from the server.
    #[serde(default)]
    pub update_keys: Vec<String>,
    /// The server's release channel.
    #[serde(default)]
    pub channel: String,
}

/// Trades an enrollment token for a device token.
pub async fn enroll(server: &ServerConfig, req: &EnrollRequest<'_>) -> Result<Enrollment> {
    let resp = server
        .http_client()?
        .post(server.https("/api/agent/v1/enroll"))
        .json(req)
        .send()
        .await?;
    let status = resp.status();
    if !status.is_success() {
        let body: serde_json::Value = resp.json().await.unwrap_or_default();
        let code = body["error"].as_str().unwrap_or("error").to_string();
        return Err(match code.as_str() {
            "invalid_enroll_token" => {
                anyhow!("the enrollment token is invalid, expired or used up")
            }
            "too_many_attempts" => anyhow!("too many failed attempts; try again in 15 minutes"),
            _ => anyhow!("enrollment failed: {status} {code}"),
        });
    }
    Ok(resp.json().await?)
}
