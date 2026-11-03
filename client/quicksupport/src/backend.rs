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

#[cfg_attr(not(feature = "core"), allow(dead_code))]
#[derive(Deserialize, Debug)]
pub struct Registration {
    pub device_id: i64,
    pub device_code: String,
    pub device_token: String,
}

#[derive(Deserialize, Debug, Clone)]
pub struct Latest {
    pub version: String,
    pub url: String,
    pub sha256: String,
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

pub fn register(cfg: &Config, lang: Lang) -> Result<Registration, String> {
    let req = RegisterReq {
        machine_id: random_id(),
        hostname: whoami::hostname().unwrap_or_else(|_| "unknown".into()),
        os_version: os_version(),
        user: whoami::username().unwrap_or_default(),
        client_version: VERSION,
        language: lang.code(),
    };
    http()
        .post(format!("{}/api/agent/v1/quick/register", cfg.server))
        .send_json(&req)
        .map_err(|e| e.to_string())?
        .body_mut()
        .read_json::<Registration>()
        .map_err(|e| e.to_string())
}

/// Tells the server the client is closing, so the code stops working at once.
pub fn close(cfg: &Config, token: &str) {
    let _ = http()
        .post(format!("{}/api/agent/v1/quick/close", cfg.server))
        .header("Authorization", format!("Bearer {token}"))
        .send_empty();
}

pub fn check_latest(cfg: &Config) -> Result<Latest, String> {
    http()
        .get(format!("{}/api/agent/v1/quick/latest", cfg.server))
        .call()
        .map_err(|e| e.to_string())?
        .body_mut()
        .read_json::<Latest>()
        .map_err(|e| e.to_string())
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

/// Downloads the new exe to the temp folder, checks its SHA-256, starts it and
/// returns, after which the caller exits. The new copy registers a new code.
pub fn download_and_start(cfg: &Config, latest: &Latest) -> Result<(), String> {
    use sha2::{Digest, Sha256};
    use std::io::Read;
    let url = if latest.url.starts_with('/') {
        format!("{}{}", cfg.server, latest.url)
    } else {
        latest.url.clone()
    };
    crate::config::validate_server(&url, cfg.insecure_dev).map_err(|e| format!("{e:?}"))?;
    let mut body = Vec::new();
    http()
        .get(&url)
        .call()
        .map_err(|e| e.to_string())?
        .body_mut()
        .as_reader()
        .take(200 * 1024 * 1024)
        .read_to_end(&mut body)
        .map_err(|e| e.to_string())?;
    let sum = hex::encode(Sha256::digest(&body));
    if !sum.eq_ignore_ascii_case(latest.sha256.trim()) {
        return Err("checksum mismatch".into());
    }
    let path = std::env::temp_dir().join(format!(
        "rhis-quicksupport-{}.exe",
        latest.version.trim_start_matches('v')
    ));
    std::fs::write(&path, &body).map_err(|e| e.to_string())?;
    std::process::Command::new(&path)
        .spawn()
        .map_err(|e| e.to_string())?;
    Ok(())
}

fn os_version() -> String {
    whoami::distro().unwrap_or_else(|_| std::env::consts::OS.to_string())
}

/// Runs the worker until the window sends `Shutdown`.
pub fn run(
    cfg: Config,
    lang: Lang,
    events: Sender<Event>,
    cmds: Receiver<Cmd>,
    wake: impl Fn() + Send + 'static,
) {
    let send = |e: Event| {
        let _ = events.send(e);
        wake();
    };
    if cfg.demo {
        return demo(send, cmds);
    }
    let mut backoff = 1u64;
    loop {
        send(Event::Connecting);
        let reg = match register(&cfg, lang) {
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
        send(Event::Ready {
            code: reg.device_code.clone(),
        });
        let stop = session_loop(&cfg, &reg, &send, &cmds);
        close(&cfg, &reg.device_token);
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
    _reg: &Registration,
    send: &impl Fn(Event),
    cmds: &Receiver<Cmd>,
) -> bool {
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
