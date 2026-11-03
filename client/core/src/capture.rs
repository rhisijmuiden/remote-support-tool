//! Screen capture interface.
use anyhow::Result;

pub use crate::protocol::Monitor;

/// One captured frame: packed BGRA, `width * 4` bytes per row, even width and height.
#[derive(Clone, Default)]
pub struct Frame {
    pub width: usize,
    pub height: usize,
    pub data: Vec<u8>,
}

pub trait Capturer: Send {
    fn monitors(&self) -> Vec<Monitor>;
    /// Index of the monitor being captured.
    fn current(&self) -> usize;
    fn select(&mut self, index: usize) -> Result<()>;
    /// Captures the current monitor into `frame`.
    fn capture(&mut self, frame: &mut Frame) -> Result<()>;
    /// Called after the input desktop changed (sign-in screen, UAC prompt); rebuilds handles.
    fn reset(&mut self) -> Result<()> {
        Ok(())
    }
}

/// Rounds a dimension down to an even number, as H.264 4:2:0 requires.
pub fn even(n: usize) -> usize {
    n & !1
}
