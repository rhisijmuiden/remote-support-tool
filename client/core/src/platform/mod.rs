//! Screen capture and input for the current operating system.
//!
//! Windows uses GDI capture and SendInput and follows the input desktop, so the sign-in
//! screen and UAC prompts work when the process runs as SYSTEM in the console session.
//! Other systems get a synthetic test screen, used for development and end-to-end tests.
use anyhow::Result;

use crate::capture::Capturer;
use crate::input::InputSink;

pub mod testscreen;
#[cfg(windows)]
pub mod windows;

/// Shared hooks the session uses on the local machine.
pub trait Platform: Send + Sync {
    fn capturer(&self) -> Result<Box<dyn Capturer>>;
    fn input(&self) -> Result<Box<dyn InputSink>>;
    /// Called on the capture thread before every frame. Returns true when the thread
    /// switched to a different desktop and capture handles must be rebuilt.
    fn follow_input_desktop(&self) -> bool {
        false
    }
    /// Shows that a remote session is active. The returned guard hides it again.
    fn session_banner(&self, _text: &str) -> Option<Box<dyn Send>> {
        None
    }
}

/// The platform this build runs on.
pub fn default_platform() -> Box<dyn Platform> {
    #[cfg(windows)]
    {
        Box::new(windows::WindowsPlatform)
    }
    #[cfg(not(windows))]
    {
        Box::new(testscreen::TestPlatform::default())
    }
}
