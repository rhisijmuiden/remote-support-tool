// SPDX-License-Identifier: AGPL-3.0-only
//! The worker thread behind the window. It registers with the server, keeps
//! the control channel open through `rhis-core`, asks the window when a
//! technician wants to connect, and runs the remote session.
//!
//! All traffic is TLS: HTTPS for these calls, WSS for the control and session
//! sockets, and DTLS-SRTP for the WebRTC screen stream.

use crate::config::Config;
use crate::i18n::Lang;
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use std::sync::mpsc::{Receiver, RecvTimeoutError, Sender};
use std::time::Duration;

pub const VERSION: &str = env!("CARGO_PKG_VERSION");

/// How long the customer has to answer before the request is declined.
pub const ANSWER_SECS: u64 = 60;

// Some variants are only produced once rhis-core drives the control channel.
#[cfg_attr(not(feature = "core"), allow(dead_code))]
#[derive(Debug, Clone)]
pub enum Event {
    Connecting,
    /// Registered with a server; the window uses this for update checks.
    Registered(crate::update::UpdateCtx),
    Ready {
        code: String,
    },
    Offline {
        retry_in: u64,
    },
    Request {
        session_id: String,
        technician: String,
        recording: bool,
    },
    RequestCancelled,
    Active {
        technician: String,
        recording: bool,
    },
    Ended {
        declined: bool,
    },
    Fatal {
        key: &'static str,
        detail: String,
    },
}

#[cfg_attr(not(feature = "core"), allow(dead_code))]
#[derive(Debug)]
pub enum Cmd {
    Answer { session_id: String, allow: bool },
    EndSession,
    Retry,
    Shutdown,
}

#[derive(Serialize)]
struct RegisterReq<'a> {
    machine_id: String,
    hostname: String,
    os_version: String,
    user: String,
    client_version: &'a str,
    language: &'a str,
}

#[derive(Deserialize, Debug)]
pub struct Registration {
    pub device_id: i64,
    #[cfg_attr(feature = "core", allow(dead_code))]
    // the code arrives again in the welcome message
    pub device_code: String,
    pub device_token: String,
}

/// Formats "123456789" as "123 456 789".
pub fn format_code(code: &str) -> String {
    let digits: Vec<char> = code.chars().filter(|c| c.is_ascii_digit()).collect();
    digits
        .chunks(3)
        .map(|c| c.iter().collect::<String>())
        .collect::<Vec<_>>()
        .join(" ")
}

pub fn http() -> ureq::Agent {
    use ureq::tls::{RootCerts, TlsConfig};
    ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .timeout_global(Some(Duration::from_secs(20)))
            .tls_config(
                TlsConfig::builder()
                    .root_certs(RootCerts::PlatformVerifier)
                    .build(),
            )
            .user_agent(format!("rhis-quicksupport/{VERSION}"))
            .build(),
    )
}

fn random_id() -> String {
    let b: [u8; 16] = rand::random();
    hex::encode(b)
}

pub fn register(
    cfg: &Config,
    lang: Lang,
    machine_id: &str,
) -> Result<(Registration, String), String> {
    let req = RegisterReq {
        machine_id: machine_id.to_string(),
        hostname: whoami::hostname().unwrap_or_else(|_| "unknown".into()),
        os_version: os_version(),
        user: whoami::username().unwrap_or_default(),
        client_version: VERSION,
        language: lang.code(),
    };
    // Try the servers in order; the first that answers is used for the whole run.
    let mut last = String::new();
    for server in std::iter::once(&cfg.server).chain(cfg.alternates.iter()) {
        match http()
            .post(format!("{server}/api/agent/v1/quick/register"))
            .send_json(&req)
            .map_err(|e| e.to_string())
            .and_then(|mut r| {
                r.body_mut()
                    .read_json::<Registration>()
                    .map_err(|e| e.to_string())
            }) {
            Ok(reg) => return Ok((reg, server.clone())),
            Err(e) => last = format!("{server}: {e}"),
        }
    }
    Err(last)
}

/// Tells the server the client is closing, so the code stops working at once.
pub fn close(server: &str, token: &str) {
    let _ = http()
        .post(format!("{server}/api/agent/v1/quick/close"))
        .header("Authorization", format!("Bearer {token}"))
        .send_empty();
}

/// Compares dotted versions ("0.2.0" > "0.1.9"); a pre-release suffix sorts lower.
pub fn is_newer(candidate: &str, current: &str) -> bool {
    fn parts(v: &str) -> (Vec<u64>, bool) {
        let v = v.trim_start_matches('v');
        let (core, pre) = v.split_once('-').map_or((v, false), |(c, _)| (c, true));
        (
            core.split('.').map(|p| p.parse().unwrap_or(0)).collect(),
            pre,
        )
    }
    let (a, apre) = parts(candidate);
    let (b, bpre) = parts(current);
    match a.cmp(&b) {
        std::cmp::Ordering::Greater => true,
        std::cmp::Ordering::Less => false,
        std::cmp::Ordering::Equal => bpre && !apre,
    }
}

fn os_version() -> String {
    whoami::distro().unwrap_or_else(|_| std::env::consts::OS.to_string())
}

/// Runs the worker until the window sends `Shutdown`.
pub type Emit = Arc<dyn Fn(Event) + Send + Sync>;

pub fn run(
    cfg: Config,
    lang: Lang,
    events: Sender<Event>,
    cmds: Receiver<Cmd>,
    wake: impl Fn() + Send + Sync + 'static,
) {
    let send: Emit = Arc::new(move |e: Event| {
        let _ = events.send(e);
        wake();
    });
    if cfg.demo {
        return demo(|e| send(e), cmds);
    }
    // A fresh identity per run: QuickSupport keeps nothing on the computer.
    let machine_id = random_id();
    let mut backoff = 1u64;
    loop {
        send(Event::Connecting);
        let (reg, server) = match register(&cfg, lang, &machine_id) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("register: {e}");
                // Count down so the window can show when the next try happens.
                for left in (1..=backoff).rev() {
                    send(Event::Offline { retry_in: left });
                    match cmds.recv_timeout(Duration::from_secs(1)) {
                        Ok(Cmd::Shutdown) | Err(RecvTimeoutError::Disconnected) => return,
                        Ok(Cmd::Retry) => break,
                        _ => {}
                    }
                }
                backoff = (backoff * 2).min(60);
                continue;
            }
        };
        backoff = 1;
        send(Event::Registered(crate::update::UpdateCtx {
            server: server.clone(),
            token: reg.device_token.clone(),
            device_id: reg.device_id,
        }));
        let stop = session_loop(&cfg, &server, &reg, &machine_id, send.clone(), &cmds);
        close(&server, &reg.device_token);
        if stop {
            return;
        }
    }
}

/// Holds the control channel and runs sessions. Returns true when the window
/// asked to shut down, false to register again.
#[cfg(not(feature = "core"))]
fn session_loop(
    _cfg: &Config,
    _server: &str,
    reg: &Registration,
    _machine_id: &str,
    send: Emit,
    cmds: &Receiver<Cmd>,
) -> bool {
    send(Event::Ready {
        code: reg.device_code.clone(),
    });
    send(Event::Fatal {
        key: "engine_missing",
        detail: String::new(),
    });
    loop {
        match cmds.recv() {
            Ok(Cmd::Shutdown) | Err(_) => return true,
            Ok(Cmd::Retry) => return false,
            _ => {}
        }
    }
}

#[cfg(feature = "core")]
use engine::session_loop;

/// The real connection, through rhis-core: the same control channel and
/// session sockets as the Windows agent (docs/agent-protocol.md).
#[cfg(feature = "core")]
mod engine {
    use super::*;
    use rhis_core::control::{self, ControlConfig, ControlHandler, Decision};
    use rhis_core::net::ServerConfig;
    use rhis_core::protocol::SessionRequest;
    use rhis_core::session::{self, SessionConfig};
    use std::sync::Mutex;
    use tokio::sync::{oneshot, watch};

    #[derive(Default)]
    struct State {
        /// A request waiting for the customer's answer.
        pending: Option<(String, oneshot::Sender<bool>)>,
        /// The running session and the signal that stops it.
        active: Option<(String, watch::Sender<bool>)>,
    }

    struct Handler {
        send: Emit,
        state: Arc<Mutex<State>>,
        token: String,
        product_name: String,
    }

    #[async_trait::async_trait]
    impl ControlHandler for Handler {
        async fn on_welcome(&self, _device_id: i64, device_code: &str) {
            // Only now can a technician reach us, so only now show the code as ready.
            (self.send)(Event::Ready {
                code: device_code.to_string(),
            });
        }

        async fn on_session_request(&self, req: SessionRequest, server: ServerConfig) -> Decision {
            if req.kind != "quick" {
                return Decision::Reject("denied".into());
            }
            let rx = {
                let mut st = self.state.lock().unwrap();
                if st.active.is_some() || st.pending.is_some() {
                    return Decision::Reject("busy".into());
                }
                let (tx, rx) = oneshot::channel();
                st.pending = Some((req.session_id.clone(), tx));
                rx
            };
            (self.send)(Event::Request {
                session_id: req.session_id.clone(),
                technician: req.user_name.clone(),
                recording: req.recording,
            });
            let answer = tokio::time::timeout(Duration::from_secs(ANSWER_SECS + 5), rx).await;
            {
                let mut st = self.state.lock().unwrap();
                if st
                    .pending
                    .as_ref()
                    .is_some_and(|(id, _)| *id == req.session_id)
                {
                    st.pending = None;
                }
            }
            match answer {
                Ok(Ok(true)) => {}
                // Cancelled by the technician: the window already shows that.
                Ok(Err(_)) => return Decision::Reject("cancelled".into()),
                // Declined, or no answer in time.
                _ => {
                    (self.send)(Event::Ended { declined: true });
                    return Decision::Reject("denied".into());
                }
            }

            // Setting `cancel` makes the session tell the viewer and stop the capture.
            let (stop, cancel) = watch::channel(false);
            self.state.lock().unwrap().active = Some((req.session_id.clone(), stop));
            (self.send)(Event::Active {
                technician: req.user_name.clone(),
                recording: req.recording,
            });
            let cfg = SessionConfig {
                server,
                token: self.token.clone(),
                request: req,
                fps: 30.0,
                product_name: self.product_name.clone(),
                cancel: Some(cancel),
            };
            let (send, state) = (self.send.clone(), self.state.clone());
            tokio::spawn(async move {
                let platform = Arc::from(rhis_core::platform::default_platform());
                match session::run(cfg, platform).await {
                    Ok(reason) => eprintln!("session ended: {reason}"),
                    Err(e) => eprintln!("session failed: {e:#}"),
                }
                state.lock().unwrap().active = None;
                send(Event::Ended { declined: false });
            });
            Decision::Accept
        }

        async fn on_session_cancel(&self, session_id: &str) {
            let mut st = self.state.lock().unwrap();
            if st.pending.as_ref().is_some_and(|(id, _)| id == session_id) {
                st.pending = None; // dropping the sender wakes the waiting request
                drop(st);
                (self.send)(Event::RequestCancelled);
            } else if let Some((id, stop)) = &st.active
                && id == session_id
            {
                let _ = stop.send(true);
            }
        }
    }

    pub fn session_loop(
        cfg: &Config,
        server_url: &str,
        reg: &Registration,
        machine_id: &str,
        send: Emit,
        cmds: &Receiver<Cmd>,
    ) -> bool {
        // The device exists on the server that registered it; that one is used.
        let server = match ServerConfig::new(server_url, None, cfg.insecure_dev) {
            Ok(s) => s,
            Err(e) => {
                send(Event::Fatal {
                    key: "not_https",
                    detail: format!("{e:#}"),
                });
                return wait_for_shutdown(cmds);
            }
        };
        let rt = match tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
        {
            Ok(rt) => rt,
            Err(e) => {
                send(Event::Fatal {
                    key: "error",
                    detail: e.to_string(),
                });
                return wait_for_shutdown(cmds);
            }
        };
        let state = Arc::new(Mutex::new(State::default()));
        let handler = Arc::new(Handler {
            send: send.clone(),
            state: state.clone(),
            token: reg.device_token.clone(),
            product_name: cfg.product_name.clone(),
        });
        let ccfg = ControlConfig {
            server,
            alternates: vec![],
            token: reg.device_token.clone(),
            machine_id: machine_id.to_string(),
            hostname: whoami::hostname().unwrap_or_else(|_| "unknown".into()),
            os_version: os_version(),
            agent_version: VERSION.to_string(),
        };
        let control = rt.spawn(control::run(ccfg, handler));
        let stop_active = || {
            if let Some((_, stop)) = &state.lock().unwrap().active {
                let _ = stop.send(true);
            }
        };
        let shutdown = loop {
            match cmds.recv_timeout(Duration::from_millis(500)) {
                Ok(Cmd::Answer { session_id, allow }) => {
                    let mut st = state.lock().unwrap();
                    if st.pending.as_ref().is_some_and(|(id, _)| *id == session_id)
                        && let Some((_, tx)) = st.pending.take()
                    {
                        let _ = tx.send(allow);
                    }
                }
                Ok(Cmd::EndSession) => stop_active(),
                Ok(Cmd::Retry) => {}
                Ok(Cmd::Shutdown) | Err(RecvTimeoutError::Disconnected) => break true,
                // control::run only returns when the server no longer knows our token
                // (for example after a long time offline): register again for a new code.
                Err(RecvTimeoutError::Timeout) if control.is_finished() => break false,
                Err(RecvTimeoutError::Timeout) => {}
            }
        };
        stop_active();
        // Give a running session a moment to say goodbye on its socket.
        std::thread::sleep(Duration::from_millis(300));
        rt.shutdown_timeout(Duration::from_secs(2));
        shutdown
    }

    fn wait_for_shutdown(cmds: &Receiver<Cmd>) -> bool {
        loop {
            match cmds.recv() {
                Ok(Cmd::Shutdown) | Err(_) => return true,
                Ok(Cmd::Retry) => return false,
                _ => {}
            }
        }
    }
}

/// Plays the whole flow without a server, for trying the window and for screenshots.
fn demo(send: impl Fn(Event), cmds: Receiver<Cmd>) {
    let wait = |secs: u64| -> Option<Cmd> {
        match cmds.recv_timeout(Duration::from_secs(secs)) {
            Ok(c) => Some(c),
            Err(RecvTimeoutError::Timeout) => None,
            Err(RecvTimeoutError::Disconnected) => Some(Cmd::Shutdown),
        }
    };
    let step = std::env::var("RHIS_QS_DEMO_STEP").unwrap_or_default();
    send(Event::Connecting);
    if step == "connecting" {
        return while !matches!(wait(3600), Some(Cmd::Shutdown)) {};
    }
    if step == "offline" {
        send(Event::Offline { retry_in: 8 });
        return while !matches!(wait(3600), Some(Cmd::Shutdown)) {};
    }
    wait(1);
    send(Event::Ready {
        code: "418207365".into(),
    });
    match step.as_str() {
        "ready" => return while !matches!(wait(3600), Some(Cmd::Shutdown)) {},
        "request" => {
            send(Event::Request {
                session_id: "demo".into(),
                technician: "Reinder".into(),
                recording: true,
            });
            return while !matches!(wait(3600), Some(Cmd::Shutdown)) {};
        }
        "active" => {
            send(Event::Active {
                technician: "Reinder".into(),
                recording: true,
            });
            return while !matches!(wait(3600), Some(Cmd::Shutdown)) {};
        }
        _ => {}
    }
    loop {
        if matches!(wait(4), Some(Cmd::Shutdown)) {
            return;
        }
        send(Event::Request {
            session_id: "demo".into(),
            technician: "Reinder".into(),
            recording: true,
        });
        let allow = loop {
            match wait(ANSWER_SECS) {
                Some(Cmd::Answer { allow, .. }) => break allow,
                Some(Cmd::Shutdown) => return,
                None => break false,
                _ => {}
            }
        };
        if !allow {
            send(Event::Ended { declined: true });
            continue;
        }
        send(Event::Active {
            technician: "Reinder".into(),
            recording: true,
        });
        loop {
            match wait(3600) {
                Some(Cmd::EndSession) => break,
                Some(Cmd::Shutdown) => return,
                _ => {}
            }
        }
        send(Event::Ended { declined: false });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn code_format() {
        assert_eq!(format_code("418207365"), "418 207 365");
        assert_eq!(format_code("418 207 365"), "418 207 365");
    }

    #[test]
    fn versions() {
        assert!(is_newer("0.2.0", "0.1.9"));
        assert!(is_newer("v1.0.0", "0.9.0"));
        assert!(!is_newer("0.1.0", "0.1.0"));
        assert!(is_newer("0.1.0", "0.1.0-test.3"));
        assert!(!is_newer("0.1.0-test.3", "0.1.0"));
        assert!(!is_newer("0.1.0", "0.2.0"));
    }
}
