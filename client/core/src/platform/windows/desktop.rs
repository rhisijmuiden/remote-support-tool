//! Keeps the capture thread on the input desktop, so the sign-in screen, the lock screen and
//! UAC prompts (the secure desktop) can be seen and used. This needs the SYSTEM account.
use std::cell::RefCell;

use windows::Win32::Foundation::{GENERIC_ALL, HANDLE};
use windows::Win32::System::StationsAndDesktops::{
    CloseDesktop, DESKTOP_ACCESS_FLAGS, DESKTOP_CONTROL_FLAGS, GetUserObjectInformationW, HDESK,
    OpenInputDesktop, SetThreadDesktop, UOI_NAME,
};

thread_local! {
    static CURRENT: RefCell<(String, Option<HDESK>)> = const { RefCell::new((String::new(), None)) };
}

fn desktop_name(h: HDESK) -> String {
    let mut buf = [0u16; 256];
    let mut needed = 0u32;
    let ok = unsafe {
        GetUserObjectInformationW(
            HANDLE(h.0),
            UOI_NAME,
            Some(buf.as_mut_ptr().cast()),
            (buf.len() * 2) as u32,
            Some(&mut needed),
        )
    };
    if ok.is_err() {
        return String::new();
    }
    let n = buf.iter().position(|&c| c == 0).unwrap_or(buf.len());
    String::from_utf16_lossy(&buf[..n])
}

/// Switches this thread to the current input desktop. Returns true when it changed.
pub fn follow_input_desktop() -> bool {
    let Ok(h) = (unsafe {
        OpenInputDesktop(
            DESKTOP_CONTROL_FLAGS(0),
            false,
            DESKTOP_ACCESS_FLAGS(GENERIC_ALL.0),
        )
    }) else {
        return false;
    };
    let name = desktop_name(h);
    CURRENT.with(|c| {
        let mut c = c.borrow_mut();
        if !name.is_empty() && name == c.0 {
            unsafe {
                let _ = CloseDesktop(h);
            }
            return false;
        }
        if unsafe { SetThreadDesktop(h) }.is_err() {
            unsafe {
                let _ = CloseDesktop(h);
            }
            return false;
        }
        if let Some(old) = c.1.replace(h) {
            unsafe {
                let _ = CloseDesktop(old);
            }
        }
        c.0 = name;
        true
    })
}
