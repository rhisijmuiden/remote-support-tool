//! Windows facts: version, MachineGuid, the console user, DPI awareness.
use windows::Win32::System::RemoteDesktop::{
    WTS_CURRENT_SERVER_HANDLE, WTS_INFO_CLASS, WTSDomainName, WTSFreeMemory,
    WTSGetActiveConsoleSessionId, WTSQuerySessionInformationW, WTSUserName,
};
use windows::Win32::UI::HiDpi::{
    DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, SetProcessDpiAwarenessContext,
};
use windows::core::PWSTR;
use winreg::RegKey;
use winreg::enums::{HKEY_LOCAL_MACHINE, KEY_READ, KEY_WOW64_64KEY};

fn hklm(path: &str) -> Option<RegKey> {
    RegKey::predef(HKEY_LOCAL_MACHINE)
        .open_subkey_with_flags(path, KEY_READ | KEY_WOW64_64KEY)
        .ok()
}

/// For example "Windows 11 Pro 24H2 (build 26100.2314)".
pub fn os_version() -> String {
    let Some(k) = hklm(r"SOFTWARE\Microsoft\Windows NT\CurrentVersion") else {
        return "Windows".into();
    };
    let mut name: String = k
        .get_value("ProductName")
        .unwrap_or_else(|_| "Windows".into());
    let build: String = k.get_value("CurrentBuild").unwrap_or_default();
    let ubr: u32 = k.get_value("UBR").unwrap_or(0);
    let display: String = k.get_value("DisplayVersion").unwrap_or_default();
    // Windows 11 still says "Windows 10" in ProductName.
    if build.parse::<u32>().unwrap_or(0) >= 22000 && name.starts_with("Windows 10") {
        name = name.replacen("Windows 10", "Windows 11", 1);
    }
    let mut s = name;
    if !display.is_empty() {
        s.push(' ');
        s.push_str(&display);
    }
    if !build.is_empty() {
        s.push_str(&format!(" (build {build}.{ubr})"));
    }
    s
}

pub fn machine_guid() -> Option<String> {
    hklm(r"SOFTWARE\Microsoft\Cryptography")?
        .get_value::<String, _>("MachineGuid")
        .ok()
        .map(|s| s.to_lowercase())
}

fn session_string(id: u32, class: WTS_INFO_CLASS) -> String {
    unsafe {
        let mut buf = PWSTR::null();
        let mut len = 0u32;
        if WTSQuerySessionInformationW(
            Some(WTS_CURRENT_SERVER_HANDLE),
            id,
            class,
            &mut buf,
            &mut len,
        )
        .is_err()
            || buf.is_null()
        {
            return String::new();
        }
        let s = buf.to_string().unwrap_or_default();
        WTSFreeMemory(buf.0.cast());
        s
    }
}

/// DOMAIN\user signed in at the console, or "" at the sign-in screen.
pub fn console_user() -> String {
    let id = unsafe { WTSGetActiveConsoleSessionId() };
    if id == u32::MAX {
        return String::new();
    }
    let user = session_string(id, WTSUserName);
    if user.is_empty() {
        return user;
    }
    let domain = session_string(id, WTSDomainName);
    if domain.is_empty() {
        user
    } else {
        format!(r"{domain}\{user}")
    }
}

/// Capture and input use physical pixels.
pub fn set_dpi_aware() {
    unsafe {
        let _ = SetProcessDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
    }
}
