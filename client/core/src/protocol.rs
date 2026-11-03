//! Messages of the agent protocol (docs/agent-protocol.md).
use serde::{Deserialize, Serialize};

/// STUN or TURN server, as the server hands it out.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct IceServer {
    pub urls: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub username: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub credential: String,
}

/// Messages from a client to the server on the control channel.
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ClientMsg {
    Hello {
        hostname: String,
        os_version: String,
        agent_version: String,
        user: String,
        machine_id: String,
    },
    Status {
        user: String,
    },
    SessionAccept {
        session_id: String,
    },
    SessionReject {
        session_id: String,
        reason: String,
    },
    Pong,
}

/// A request from a technician to open a session.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionRequest {
    pub session_id: String,
    /// `unattended`, `attended` or `quick`.
    pub kind: String,
    #[serde(default)]
    pub user_name: String,
    #[serde(default)]
    pub recording: bool,
    #[serde(default)]
    pub ice_servers: Vec<IceServer>,
}

/// Messages from the server on the control channel.
#[derive(Debug, Clone, PartialEq)]
pub enum ServerMsg {
    Welcome {
        device_id: i64,
        device_code: String,
        ping_interval: u64,
    },
    SessionRequest(SessionRequest),
    SessionCancel {
        session_id: String,
    },
    Ping,
    /// Any other message (for example `update_available`), passed on as JSON.
    Other(serde_json::Value),
}

impl ServerMsg {
    pub fn parse(text: &str) -> anyhow::Result<Self> {
        let v: serde_json::Value = serde_json::from_str(text)?;
        let t = v.get("type").and_then(|t| t.as_str()).unwrap_or_default();
        Ok(match t {
            "welcome" => ServerMsg::Welcome {
                device_id: v["device_id"].as_i64().unwrap_or_default(),
                device_code: v["device_code"].as_str().unwrap_or_default().to_string(),
                ping_interval: v["ping_interval"].as_u64().unwrap_or(25),
            },
            "session_request" => ServerMsg::SessionRequest(serde_json::from_value(v)?),
            "session_cancel" => ServerMsg::SessionCancel {
                session_id: v["session_id"].as_str().unwrap_or_default().to_string(),
            },
            "ping" => ServerMsg::Ping,
            _ => ServerMsg::Other(v),
        })
    }
}

/// One monitor of the remote machine.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct Monitor {
    pub index: usize,
    pub name: String,
    /// Position in the virtual desktop, in physical pixels.
    pub x: i32,
    pub y: i32,
    pub width: u32,
    pub height: u32,
    pub primary: bool,
}

/// Text frames on the session socket.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Signal {
    Ready {
        #[serde(default)]
        peer: String,
    },
    Offer {
        sdp: String,
    },
    Answer {
        sdp: String,
    },
    Ice {
        candidate: Option<String>,
        #[serde(default)]
        sdp_mid: Option<String>,
        #[serde(default)]
        sdp_mline_index: Option<u16>,
    },
    Relay,
    Keyframe,
    Input {
        event: InputEvent,
    },
    Screen {
        width: u32,
        height: u32,
        monitor: usize,
        monitors: Vec<Monitor>,
    },
    Transport {
        transport: String,
    },
    End {
        #[serde(default)]
        reason: String,
    },
    #[serde(other)]
    Unknown,
}

/// Input from the technician. Coordinates are 0..1 of the shown monitor.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(tag = "t", rename_all = "snake_case")]
pub enum InputEvent {
    Move {
        x: f64,
        y: f64,
    },
    Button {
        button: u8,
        down: bool,
    },
    Wheel {
        dx: f64,
        dy: f64,
    },
    Key {
        code: String,
        down: bool,
    },
    /// Release every key and button that is still down (the viewer lost focus).
    ReleaseAll,
    /// Ctrl+Alt+Del.
    Cad,
    Monitor {
        index: usize,
    },
}

/// Flags byte of a relay video frame.
pub const RELAY_KEYFRAME: u8 = 1;

/// Builds a binary relay frame: flags, timestamp (µs, big endian), H.264 Annex B access unit.
pub fn relay_frame(key: bool, ts_us: u64, data: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(9 + data.len());
    out.push(if key { RELAY_KEYFRAME } else { 0 });
    out.extend_from_slice(&ts_us.to_be_bytes());
    out.extend_from_slice(data);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_server_messages() {
        let m = ServerMsg::parse(r#"{"type":"session_request","session_id":"s1","kind":"unattended","user_name":"Tess","recording":true,"ice_servers":[{"urls":["stun:x:3478"]}]}"#).unwrap();
        match m {
            ServerMsg::SessionRequest(r) => {
                assert_eq!(r.session_id, "s1");
                assert!(r.recording);
                assert_eq!(r.ice_servers[0].urls[0], "stun:x:3478");
            }
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            ServerMsg::parse(r#"{"type":"update_available","version":"1"}"#).unwrap(),
            ServerMsg::Other(_)
        ));
    }

    #[test]
    fn signals_round_trip() {
        let s: Signal =
            serde_json::from_str(r#"{"type":"input","event":{"t":"move","x":0.5,"y":0.25}}"#)
                .unwrap();
        assert_eq!(
            s,
            Signal::Input {
                event: InputEvent::Move { x: 0.5, y: 0.25 }
            }
        );
        let s: Signal = serde_json::from_str(r#"{"type":"ice","candidate":null}"#).unwrap();
        assert_eq!(
            s,
            Signal::Ice {
                candidate: None,
                sdp_mid: None,
                sdp_mline_index: None
            }
        );
        let s: Signal = serde_json::from_str(r#"{"type":"something_new"}"#).unwrap();
        assert_eq!(s, Signal::Unknown);
        let hello = serde_json::to_string(&ClientMsg::Pong).unwrap();
        assert_eq!(hello, r#"{"type":"pong"}"#);
    }

    #[test]
    fn relay_frame_layout() {
        let f = relay_frame(true, 0x0102, b"ab");
        assert_eq!(f, vec![1, 0, 0, 0, 0, 0, 0, 1, 2, b'a', b'b']);
    }
}
