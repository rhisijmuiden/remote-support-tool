//! An always-on-top banner that tells the local user a remote session is active
//! (and whether it is recorded). It is hidden from screen capture, so the technician does not see it.
use std::sync::mpsc;

use windows::Win32::Foundation::{COLORREF, HWND, LPARAM, LRESULT, RECT, WPARAM};
use windows::Win32::Graphics::Gdi::{
    BeginPaint, CreateFontW, CreateSolidBrush, DT_CENTER, DT_SINGLELINE, DT_VCENTER, DeleteObject,
    DrawTextW, EndPaint, FillRect, HGDIOBJ, PAINTSTRUCT, SelectObject, SetBkMode, SetTextColor,
    TRANSPARENT,
};
use windows::Win32::System::LibraryLoader::GetModuleHandleW;
use windows::Win32::UI::WindowsAndMessaging::{
    CS_HREDRAW, CS_VREDRAW, CreateWindowExW, DefWindowProcW, DestroyWindow, DispatchMessageW,
    GWLP_USERDATA, GetClientRect, GetMessageW, GetSystemMetrics, GetWindowLongPtrW, HTCAPTION, MSG,
    PostMessageW, PostQuitMessage, RegisterClassW, SM_CXSCREEN, SW_SHOWNOACTIVATE,
    SetWindowDisplayAffinity, SetWindowLongPtrW, ShowWindow, TranslateMessage,
    WDA_EXCLUDEFROMCAPTURE, WM_CLOSE, WM_DESTROY, WM_NCHITTEST, WM_PAINT, WNDCLASSW,
    WS_EX_NOACTIVATE, WS_EX_TOOLWINDOW, WS_EX_TOPMOST, WS_POPUP,
};
use windows::core::{PCWSTR, w};

pub struct Banner {
    hwnd: isize,
}

impl Drop for Banner {
    fn drop(&mut self) {
        unsafe {
            let _ = PostMessageW(
                Some(HWND(self.hwnd as *mut _)),
                WM_CLOSE,
                WPARAM(0),
                LPARAM(0),
            );
        }
    }
}

unsafe extern "system" fn wndproc(hwnd: HWND, msg: u32, wp: WPARAM, lp: LPARAM) -> LRESULT {
    unsafe {
        match msg {
            WM_PAINT => {
                let mut ps = PAINTSTRUCT::default();
                let dc = BeginPaint(hwnd, &mut ps);
                let mut rc = RECT::default();
                let _ = GetClientRect(hwnd, &mut rc);
                // RHIS coral background, night-blue text.
                let brush = CreateSolidBrush(COLORREF(0x0057_6bff));
                FillRect(dc, &rc, brush);
                let _ = DeleteObject(brush.into());
                let font = CreateFontW(
                    -15,
                    0,
                    0,
                    0,
                    700,
                    0,
                    0,
                    0,
                    Default::default(),
                    Default::default(),
                    Default::default(),
                    Default::default(),
                    0,
                    w!("Segoe UI"),
                );
                let old = SelectObject(dc, HGDIOBJ(font.0));
                SetBkMode(dc, TRANSPARENT);
                SetTextColor(dc, COLORREF(0x003d_2114));
                let text = GetWindowLongPtrW(hwnd, GWLP_USERDATA) as *const Vec<u16>;
                if !text.is_null() {
                    let mut t = (*text).clone();
                    DrawTextW(dc, &mut t, &mut rc, DT_CENTER | DT_VCENTER | DT_SINGLELINE);
                }
                SelectObject(dc, old);
                let _ = DeleteObject(HGDIOBJ(font.0));
                let _ = EndPaint(hwnd, &ps);
                LRESULT(0)
            }
            // Let the user drag the banner out of the way.
            WM_NCHITTEST => LRESULT(HTCAPTION as isize),
            WM_CLOSE => {
                let _ = DestroyWindow(hwnd);
                LRESULT(0)
            }
            WM_DESTROY => {
                let text = GetWindowLongPtrW(hwnd, GWLP_USERDATA) as *mut Vec<u16>;
                if !text.is_null() {
                    drop(Box::from_raw(text));
                    SetWindowLongPtrW(hwnd, GWLP_USERDATA, 0);
                }
                PostQuitMessage(0);
                LRESULT(0)
            }
            _ => DefWindowProcW(hwnd, msg, wp, lp),
        }
    }
}

/// Shows the banner on its own thread until the returned value is dropped.
pub fn show(text: &str) -> Option<Banner> {
    let text: Vec<u16> = text.encode_utf16().collect();
    let (tx, rx) = mpsc::channel::<isize>();
    std::thread::Builder::new()
        .name("rhis-banner".into())
        .spawn(move || unsafe {
            let hinst = GetModuleHandleW(None).unwrap_or_default();
            let class = w!("RHISRemoteBanner");
            let wc = WNDCLASSW {
                style: CS_HREDRAW | CS_VREDRAW,
                lpfnWndProc: Some(wndproc),
                hInstance: hinst.into(),
                lpszClassName: class,
                ..Default::default()
            };
            RegisterClassW(&wc);
            let width = (text.len() as i32 * 8 + 48).clamp(260, 640);
            let x = (GetSystemMetrics(SM_CXSCREEN) - width) / 2;
            let hwnd = CreateWindowExW(
                WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE,
                class,
                PCWSTR::null(),
                WS_POPUP,
                x,
                0,
                width,
                30,
                None,
                None,
                Some(hinst.into()),
                None,
            );
            let Ok(hwnd) = hwnd else {
                let _ = tx.send(0);
                return;
            };
            SetWindowLongPtrW(hwnd, GWLP_USERDATA, Box::into_raw(Box::new(text)) as isize);
            let _ = SetWindowDisplayAffinity(hwnd, WDA_EXCLUDEFROMCAPTURE);
            let _ = ShowWindow(hwnd, SW_SHOWNOACTIVATE);
            let _ = tx.send(hwnd.0 as isize);
            let mut msg = MSG::default();
            while GetMessageW(&mut msg, None, 0, 0).as_bool() {
                let _ = TranslateMessage(&msg);
                DispatchMessageW(&msg);
            }
        })
        .ok()?;
    match rx.recv() {
        Ok(h) if h != 0 => Some(Banner { hwnd: h }),
        _ => None,
    }
}
