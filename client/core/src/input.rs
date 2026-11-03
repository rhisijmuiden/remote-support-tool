//! Input injection interface.
use anyhow::Result;

use crate::protocol::{InputEvent, Monitor};

pub trait InputSink: Send {
    /// Applies one event. Coordinates in `ev` are relative to `monitor`.
    fn handle(&mut self, ev: &InputEvent, monitor: &Monitor) -> Result<()>;
}
