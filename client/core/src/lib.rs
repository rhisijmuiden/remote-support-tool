//! RHIS Remote client core, shared by the Windows agent and the QuickSupport client.
//!
//! - [`net`]: TLS-only connections to the server (HTTPS and WSS), optional certificate pinning.
//! - [`control`]: the long-lived control channel (presence and session requests).
//! - [`session`]: one remote session: screen capture, H.264, WebRTC with a WebSocket relay fallback, input.
//! - [`platform`]: screen capture and input injection for the current OS.
//!
//! The wire protocol is documented in `docs/agent-protocol.md`.
// SPDX-License-Identifier: AGPL-3.0-only

pub mod capture;
pub mod control;
pub mod encode;
pub mod input;
pub mod net;
pub mod platform;
pub mod protocol;
pub mod session;

/// Version of this client build.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
