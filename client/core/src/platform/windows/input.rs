//! Input injection with SendInput (absolute mouse over the virtual desktop, scan-code keys).
use std::collections::HashSet;

use anyhow::Result;
use windows::Win32::Foundation::{FARPROC, HMODULE};
use windows::Win32::System::LibraryLoader::{GetProcAddress, LoadLibraryW};
use windows::Win32::System::Threading::{EVENT_MODIFY_STATE, OpenEventW, SetEvent};
use windows::Win32::UI::Input::KeyboardAndMouse::{
    INPUT, INPUT_0, INPUT_KEYBOARD, INPUT_MOUSE, KEYBD_EVENT_FLAGS, KEYBDINPUT,
    KEYEVENTF_EXTENDEDKEY, KEYEVENTF_KEYUP, KEYEVENTF_SCANCODE, MOUSE_EVENT_FLAGS,
    MOUSEEVENTF_ABSOLUTE, MOUSEEVENTF_HWHEEL, MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP,
    MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_MIDDLEUP, MOUSEEVENTF_MOVE, MOUSEEVENTF_RIGHTDOWN,
    MOUSEEVENTF_RIGHTUP, MOUSEEVENTF_VIRTUALDESK, MOUSEEVENTF_WHEEL, MOUSEINPUT, SendInput,
    VIRTUAL_KEY,
};
use windows::Win32::UI::WindowsAndMessaging::{
    GetSystemMetrics, SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN, SM_XVIRTUALSCREEN, SM_YVIRTUALSCREEN,
};
use windows::core::{s, w};

use super::keymap::scancode;
use crate::input::InputSink;
use crate::protocol::{InputEvent, Monitor};

/// Name of the event the agent service waits on to send Ctrl+Alt+Del.
pub const SAS_EVENT: &str = r"Global\RHISRemoteSAS";

#[derive(Default)]
pub struct SendInputSink {
    keys: HashSet<String>,
    buttons: HashSet<u8>,
}

fn send(inputs: &[INPUT]) {
    unsafe {
        SendInput(inputs, std::mem::size_of::<INPUT>() as i32);
    }
}

fn mouse(dx: i32, dy: i32, data: i32, flags: MOUSE_EVENT_FLAGS) -> INPUT {
    INPUT {
        r#type: INPUT_MOUSE,
        Anonymous: INPUT_0 {
            mi: MOUSEINPUT {
                dx,
                dy,
                mouseData: data as u32,
                dwFlags: flags,
                time: 0,
                dwExtraInfo: 0,
            },
        },
    }
}

fn key(scan: u16, ext: bool, up: bool) -> INPUT {
    let mut f = KEYEVENTF_SCANCODE;
    if ext {
        f |= KEYEVENTF_EXTENDEDKEY;
    }
    if up {
        f |= KEYEVENTF_KEYUP;
    }
    INPUT {
        r#type: INPUT_KEYBOARD,
        Anonymous: INPUT_0 {
            ki: KEYBDINPUT {
                wVk: VIRTUAL_KEY(0),
                wScan: scan,
                dwFlags: KEYBD_EVENT_FLAGS(f.0),
                time: 0,
                dwExtraInfo: 0,
            },
        },
    }
}

fn button_flags(b: u8, down: bool) -> MOUSE_EVENT_FLAGS {
    match (b, down) {
        (1, true) => MOUSEEVENTF_MIDDLEDOWN,
        (1, false) => MOUSEEVENTF_MIDDLEUP,
        (2, true) => MOUSEEVENTF_RIGHTDOWN,
        (2, false) => MOUSEEVENTF_RIGHTUP,
        (_, true) => MOUSEEVENTF_LEFTDOWN,
        (_, false) => MOUSEEVENTF_LEFTUP,
    }
}

impl InputSink for SendInputSink {
    fn handle(&mut self, ev: &InputEvent, m: &Monitor) -> Result<()> {
        match ev {
            InputEvent::Move { x, y } => {
                let (vx, vy, vw, vh) = unsafe {
                    (
                        GetSystemMetrics(SM_XVIRTUALSCREEN),
                        GetSystemMetrics(SM_YVIRTUALSCREEN),
                        GetSystemMetrics(SM_CXVIRTUALSCREEN).max(2),
                        GetSystemMetrics(SM_CYVIRTUALSCREEN).max(2),
                    )
                };
                let px = m.x as f64 + x.clamp(0.0, 1.0) * (m.width.max(1) - 1) as f64;
                let py = m.y as f64 + y.clamp(0.0, 1.0) * (m.height.max(1) - 1) as f64;
                let ax = ((px - vx as f64) * 65535.0 / (vw - 1) as f64).round() as i32;
                let ay = ((py - vy as f64) * 65535.0 / (vh - 1) as f64).round() as i32;
                send(&[mouse(
                    ax,
                    ay,
                    0,
                    MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE | MOUSEEVENTF_VIRTUALDESK,
                )]);
            }
            InputEvent::Button { button, down } => {
                if *down {
                    self.buttons.insert(*button);
                } else {
                    self.buttons.remove(button);
                }
                send(&[mouse(0, 0, 0, button_flags(*button, *down))]);
            }
            InputEvent::Wheel { dx, dy } => {
                // Browser pixels to wheel units: about 100 px per notch of 120.
                let v = (-dy * 1.2).round().clamp(-1200.0, 1200.0) as i32;
                let h = (dx * 1.2).round().clamp(-1200.0, 1200.0) as i32;
                if v != 0 {
                    send(&[mouse(0, 0, v, MOUSEEVENTF_WHEEL)]);
                }
                if h != 0 {
                    send(&[mouse(0, 0, h, MOUSEEVENTF_HWHEEL)]);
                }
            }
            InputEvent::Key { code, down } => {
                if let Some((sc, ext)) = scancode(code) {
                    if *down {
                        self.keys.insert(code.clone());
                    } else {
                        self.keys.remove(code);
                    }
                    send(&[key(sc, ext, !down)]);
                }
            }
            InputEvent::ReleaseAll => {
                for code in self.keys.drain() {
                    if let Some((sc, ext)) = scancode(&code) {
                        send(&[key(sc, ext, true)]);
                    }
                }
                for b in self.buttons.drain() {
                    send(&[mouse(0, 0, 0, button_flags(b, false))]);
                }
            }
            InputEvent::Cad => send_sas(),
            InputEvent::Monitor { .. } => {}
        }
        Ok(())
    }
}

/// Ctrl+Alt+Del: signal the service, which may call SendSAS; otherwise try SendSAS here.
pub fn send_sas() {
    unsafe {
        if let Ok(ev) = OpenEventW(EVENT_MODIFY_STATE, false, w!(r"Global\RHISRemoteSAS")) {
            let _ = SetEvent(ev);
            let _ = windows::Win32::Foundation::CloseHandle(ev);
            return;
        }
        call_send_sas();
    }
}

/// Calls SendSAS(FALSE) from sas.dll. Needs a service, or SoftwareSASGeneration set by the installer.
pub fn call_send_sas() {
    unsafe {
        let Ok(lib): Result<HMODULE, _> = LoadLibraryW(w!("sas.dll")) else {
            return;
        };
        let f: FARPROC = GetProcAddress(lib, s!("SendSAS"));
        if let Some(f) = f {
            let f: extern "system" fn(i32) = std::mem::transmute(f);
            f(0);
        }
    }
}
