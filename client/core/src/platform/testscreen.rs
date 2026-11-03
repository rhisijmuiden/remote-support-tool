//! A synthetic screen for development and tests on systems without a capture backend.
//! It draws a moving pattern and shows where the technician's pointer is and what was typed,
//! so an end-to-end test can see that video and input both work.
use std::sync::{Arc, Mutex};
use std::time::Instant;

use anyhow::Result;

use crate::capture::{Capturer, Frame, Monitor};
use crate::input::InputSink;
use crate::platform::Platform;
use crate::protocol::InputEvent;

#[derive(Default)]
pub struct TestState {
    pub x: f64,
    pub y: f64,
    pub buttons: u8,
    pub keys: Vec<String>,
    pub clicks: u32,
}

#[derive(Default, Clone)]
pub struct TestPlatform {
    pub state: Arc<Mutex<TestState>>,
}

impl Platform for TestPlatform {
    fn capturer(&self) -> Result<Box<dyn Capturer>> {
        Ok(Box::new(TestCapturer {
            state: self.state.clone(),
            start: Instant::now(),
            monitor: 0,
        }))
    }
    fn input(&self) -> Result<Box<dyn InputSink>> {
        Ok(Box::new(TestInput {
            state: self.state.clone(),
        }))
    }
}

pub struct TestCapturer {
    state: Arc<Mutex<TestState>>,
    start: Instant,
    monitor: usize,
}

const SIZES: [(u32, u32); 2] = [(1280, 720), (1024, 768)];

impl Capturer for TestCapturer {
    fn monitors(&self) -> Vec<Monitor> {
        SIZES
            .iter()
            .enumerate()
            .map(|(i, (w, h))| Monitor {
                index: i,
                name: format!("Test screen {}", i + 1),
                x: if i == 0 { 0 } else { 1280 },
                y: 0,
                width: *w,
                height: *h,
                primary: i == 0,
            })
            .collect()
    }
    fn current(&self) -> usize {
        self.monitor
    }
    fn select(&mut self, index: usize) -> Result<()> {
        if index < SIZES.len() {
            self.monitor = index;
        }
        Ok(())
    }
    fn capture(&mut self, f: &mut Frame) -> Result<()> {
        let (w, h) = (
            SIZES[self.monitor].0 as usize,
            SIZES[self.monitor].1 as usize,
        );
        f.width = w;
        f.height = h;
        f.data.resize(w * h * 4, 0);
        let t = self.start.elapsed().as_secs_f64();
        let st = self.state.lock().unwrap();
        // Background in the house colours: a dark navy gradient.
        for y in 0..h {
            for x in 0..w {
                let i = (y * w + x) * 4;
                f.data[i] = (60 + y * 40 / h) as u8; // B
                f.data[i + 1] = (28 + x * 20 / w) as u8; // G
                f.data[i + 2] = 22; // R
                f.data[i + 3] = 255;
            }
        }
        // A coral bar that moves, so the picture is visibly live.
        let bx = ((t * 160.0) as usize) % (w.saturating_sub(120).max(1));
        rect(f, bx, h / 2 - 20, 120, 40, [87, 107, 255]);
        // One block per key typed (up to 40), and the click counter as blocks at the top.
        for (i, _) in st.keys.iter().rev().take(40).enumerate() {
            rect(f, 20 + i * 18, h - 40, 14, 20, [235, 235, 235]);
        }
        for i in 0..(st.clicks.min(60) as usize) {
            rect(f, 20 + i * 10, 20, 6, 6, [120, 220, 120]);
        }
        // The remote pointer.
        let (px, py) = ((st.x * w as f64) as usize, (st.y * h as f64) as usize);
        let c = if st.buttons != 0 {
            [120, 220, 120]
        } else {
            [255, 255, 255]
        };
        rect(f, px.saturating_sub(6), py.saturating_sub(6), 12, 12, c);
        Ok(())
    }
}

fn rect(f: &mut Frame, x: usize, y: usize, w: usize, h: usize, bgr: [u8; 3]) {
    for yy in y..(y + h).min(f.height) {
        for xx in x..(x + w).min(f.width) {
            let i = (yy * f.width + xx) * 4;
            f.data[i..i + 3].copy_from_slice(&bgr);
        }
    }
}

pub struct TestInput {
    state: Arc<Mutex<TestState>>,
}

impl InputSink for TestInput {
    fn handle(&mut self, ev: &InputEvent, _m: &Monitor) -> Result<()> {
        let mut st = self.state.lock().unwrap();
        match ev {
            InputEvent::Move { x, y } => {
                st.x = x.clamp(0.0, 1.0);
                st.y = y.clamp(0.0, 1.0);
            }
            InputEvent::Button { button, down } => {
                if *down {
                    st.buttons |= 1 << button;
                    st.clicks += 1;
                } else {
                    st.buttons &= !(1 << button);
                }
            }
            InputEvent::Key { code, down: true } => st.keys.push(code.clone()),
            InputEvent::ReleaseAll => st.buttons = 0,
            _ => {}
        }
        Ok(())
    }
}
