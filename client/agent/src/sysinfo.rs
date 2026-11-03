//! Facts about this computer that the portal shows.
pub struct Info {
    pub hostname: String,
    pub os_version: String,
}

pub fn collect() -> Info {
    let hostname = hostname::get().map(|h| h.to_string_lossy().into_owned()).unwrap_or_else(|_| "unknown".into());
    Info { hostname, os_version: os_version() }
}

#[cfg(windows)]
fn os_version() -> String {
    crate::win::sys::os_version()
}

#[cfg(not(windows))]
fn os_version() -> String {
    std::fs::read_to_string("/etc/os-release")
        .ok()
        .and_then(|s| {
            s.lines().find_map(|l| l.strip_prefix("PRETTY_NAME=").map(|v| v.trim_matches('"').to_string()))
        })
        .unwrap_or_else(|| std::env::consts::OS.to_string())
}

/// A stable ID for this machine: the Windows MachineGuid, or /etc/machine-id.
pub fn machine_id() -> String {
    #[cfg(windows)]
    if let Some(id) = crate::win::sys::machine_guid() {
        return id;
    }
    #[cfg(not(windows))]
    if let Ok(id) = std::fs::read_to_string("/etc/machine-id") {
        let id = id.trim();
        if !id.is_empty() {
            return id.to_string();
        }
    }
    format!("{:032x}", rand::random::<u128>())
}

/// The user signed in at the console, as DOMAIN\user, or "".
pub fn console_user() -> String {
    #[cfg(windows)]
    {
        crate::win::sys::console_user()
    }
    #[cfg(not(windows))]
    {
        std::env::var("USER").unwrap_or_default()
    }
}
