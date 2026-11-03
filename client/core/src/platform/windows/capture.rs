//! GDI screen capture of one monitor, with the mouse pointer drawn in.
use anyhow::{Result, anyhow, bail};
use windows::Win32::Foundation::{LPARAM, RECT};
use windows::Win32::Graphics::Gdi::{
    BI_RGB, BITMAPINFO, BITMAPINFOHEADER, BitBlt, CAPTUREBLT, CreateCompatibleDC, CreateDIBSection,
    DIB_RGB_COLORS, DeleteDC, DeleteObject, EnumDisplayMonitors, GdiFlush, GetDC, GetMonitorInfoW,
    HBITMAP, HDC, HGDIOBJ, HMONITOR, MONITORINFOEXW, ReleaseDC, SRCCOPY, SelectObject,
};
use windows::Win32::UI::WindowsAndMessaging::{
    CURSOR_SHOWING, CURSORINFO, DI_NORMAL, DrawIconEx, GetCursorInfo, GetIconInfo, ICONINFO,
    MONITORINFOF_PRIMARY,
};
use windows::core::BOOL;

use crate::capture::{Capturer, Frame, Monitor, even};

pub struct GdiCapturer {
    monitors: Vec<Monitor>,
    current: usize,
    screen: HDC,
    mem: HDC,
    bmp: HBITMAP,
    old: HGDIOBJ,
    bits: *mut u8,
    size: (i32, i32),
}

// The capturer is created and used on the capture thread only.
unsafe impl Send for GdiCapturer {}

unsafe extern "system" fn monitor_cb(h: HMONITOR, _dc: HDC, _r: *mut RECT, data: LPARAM) -> BOOL {
    let list = unsafe { &mut *(data.0 as *mut Vec<Monitor>) };
    let mut mi = MONITORINFOEXW::default();
    mi.monitorInfo.cbSize = std::mem::size_of::<MONITORINFOEXW>() as u32;
    if unsafe { GetMonitorInfoW(h, &mut mi.monitorInfo as *mut _) }.as_bool() {
        let r = mi.monitorInfo.rcMonitor;
        let n = mi
            .szDevice
            .iter()
            .position(|&c| c == 0)
            .unwrap_or(mi.szDevice.len());
        list.push(Monitor {
            index: 0,
            name: String::from_utf16_lossy(&mi.szDevice[..n])
                .trim_start_matches(r"\\.\")
                .to_string(),
            x: r.left,
            y: r.top,
            width: (r.right - r.left) as u32,
            height: (r.bottom - r.top) as u32,
            primary: mi.monitorInfo.dwFlags & MONITORINFOF_PRIMARY != 0,
        });
    }
    BOOL(1)
}

/// The monitors, main monitor first.
pub fn list_monitors() -> Vec<Monitor> {
    let mut v: Vec<Monitor> = Vec::new();
    unsafe {
        let _ = EnumDisplayMonitors(
            None,
            None,
            Some(monitor_cb),
            LPARAM(&mut v as *mut _ as isize),
        );
    }
    v.sort_by_key(|m| (!m.primary, m.x, m.y));
    for (i, m) in v.iter_mut().enumerate() {
        m.index = i;
    }
    v
}

impl GdiCapturer {
    pub fn new() -> Result<Self> {
        let monitors = list_monitors();
        if monitors.is_empty() {
            bail!("no monitors found");
        }
        Ok(Self {
            monitors,
            current: 0,
            screen: HDC::default(),
            mem: HDC::default(),
            bmp: HBITMAP::default(),
            old: HGDIOBJ::default(),
            bits: std::ptr::null_mut(),
            size: (0, 0),
        })
    }

    fn release(&mut self) {
        unsafe {
            if !self.mem.is_invalid() {
                SelectObject(self.mem, self.old);
                let _ = DeleteObject(self.bmp.into());
                let _ = DeleteDC(self.mem);
            }
            if !self.screen.is_invalid() {
                ReleaseDC(None, self.screen);
            }
        }
        self.screen = HDC::default();
        self.mem = HDC::default();
        self.bmp = HBITMAP::default();
        self.bits = std::ptr::null_mut();
        self.size = (0, 0);
    }

    fn ensure(&mut self, w: i32, h: i32) -> Result<()> {
        if !self.mem.is_invalid() && self.size == (w, h) {
            return Ok(());
        }
        self.release();
        unsafe {
            self.screen = GetDC(None);
            if self.screen.is_invalid() {
                bail!("GetDC failed");
            }
            self.mem = CreateCompatibleDC(Some(self.screen));
            let bi = BITMAPINFO {
                bmiHeader: BITMAPINFOHEADER {
                    biSize: std::mem::size_of::<BITMAPINFOHEADER>() as u32,
                    biWidth: w,
                    biHeight: -h, // top-down rows
                    biPlanes: 1,
                    biBitCount: 32,
                    biCompression: BI_RGB.0,
                    ..Default::default()
                },
                ..Default::default()
            };
            let mut bits: *mut core::ffi::c_void = std::ptr::null_mut();
            self.bmp = CreateDIBSection(Some(self.mem), &bi, DIB_RGB_COLORS, &mut bits, None, 0)
                .map_err(|e| anyhow!("CreateDIBSection: {e}"))?;
            self.old = SelectObject(self.mem, self.bmp.into());
            self.bits = bits.cast();
        }
        self.size = (w, h);
        Ok(())
    }

    fn draw_cursor(&self, m: &Monitor) {
        unsafe {
            let mut ci = CURSORINFO {
                cbSize: std::mem::size_of::<CURSORINFO>() as u32,
                ..Default::default()
            };
            if GetCursorInfo(&mut ci).is_err() || ci.flags.0 & CURSOR_SHOWING.0 == 0 {
                return;
            }
            let mut ii = ICONINFO::default();
            if GetIconInfo(ci.hCursor.into(), &mut ii).is_err() {
                return;
            }
            let x = ci.ptScreenPos.x - m.x - ii.xHotspot as i32;
            let y = ci.ptScreenPos.y - m.y - ii.yHotspot as i32;
            let _ = DrawIconEx(self.mem, x, y, ci.hCursor.into(), 0, 0, 0, None, DI_NORMAL);
            if !ii.hbmMask.is_invalid() {
                let _ = DeleteObject(ii.hbmMask.into());
            }
            if !ii.hbmColor.is_invalid() {
                let _ = DeleteObject(ii.hbmColor.into());
            }
        }
    }
}

impl Drop for GdiCapturer {
    fn drop(&mut self) {
        self.release();
    }
}

impl Capturer for GdiCapturer {
    fn monitors(&self) -> Vec<Monitor> {
        list_monitors()
    }
    fn current(&self) -> usize {
        self.current
    }
    fn select(&mut self, index: usize) -> Result<()> {
        self.monitors = list_monitors();
        if index < self.monitors.len() {
            self.current = index;
        }
        Ok(())
    }
    fn reset(&mut self) -> Result<()> {
        self.release();
        self.monitors = list_monitors();
        if self.current >= self.monitors.len() {
            self.current = 0;
        }
        Ok(())
    }
    fn capture(&mut self, f: &mut Frame) -> Result<()> {
        let m = self
            .monitors
            .get(self.current)
            .cloned()
            .ok_or_else(|| anyhow!("monitor gone"))?;
        let (w, h) = (
            even(m.width as usize) as i32,
            even(m.height as usize) as i32,
        );
        if w < 2 || h < 2 {
            bail!("monitor has no size");
        }
        self.ensure(w, h)?;
        unsafe {
            BitBlt(
                self.mem,
                0,
                0,
                w,
                h,
                Some(self.screen),
                m.x,
                m.y,
                SRCCOPY | CAPTUREBLT,
            )
            .map_err(|e| anyhow!("BitBlt: {e}"))?;
        }
        self.draw_cursor(&m);
        unsafe {
            let _ = GdiFlush();
        }
        let len = (w * h * 4) as usize;
        f.width = w as usize;
        f.height = h as usize;
        f.data.resize(len, 0);
        // SAFETY: the DIB section holds w*h 32-bit pixels and lives until release().
        f.data
            .copy_from_slice(unsafe { std::slice::from_raw_parts(self.bits, len) });
        Ok(())
    }
}
