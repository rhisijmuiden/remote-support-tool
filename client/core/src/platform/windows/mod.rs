//! Windows: GDI screen capture, SendInput, input-desktop tracking and the session banner.
mod banner;
mod capture;
mod desktop;
mod input;
mod keymap;

use anyhow::Result;

use crate::capture::Capturer;
use crate::input::InputSink;
use crate::platform::Platform;

#[derive(Default)]
pub struct WindowsPlatform;

impl Platform for WindowsPlatform {
    fn capturer(&self) -> Result<Box<dyn Capturer>> {
        Ok(Box::new(capture::GdiCapturer::new()?))
    }
    fn input(&self) -> Result<Box<dyn InputSink>> {
        Ok(Box::new(input::SendInputSink::default()))
    }
    fn follow_input_desktop(&self) -> bool {
        desktop::follow_input_desktop()
    }
    fn session_banner(&self, text: &str) -> Option<Box<dyn Send>> {
        banner::show(text).map(|b| Box::new(b) as Box<dyn Send>)
    }
}

pub use input::{SAS_EVENT, call_send_sas};

/// Asks the agent service to send Ctrl+Alt+Del (only a service may call SendSAS).
/// Falls back to calling SendSAS directly when no service is listening.
pub fn send_sas() {
    input::send_sas();
}
