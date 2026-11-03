//! Starts the session helper in the console session with the service's SYSTEM token, so it can
//! capture the user's desktop, the sign-in screen and UAC prompts.
use anyhow::{Context, Result, anyhow, bail};
use rhis_core::net::ServerConfig;
use rhis_core::protocol::SessionRequest;
use windows::Win32::Foundation::{CloseHandle, HANDLE};
use windows::Win32::Security::{
    DuplicateTokenEx, SecurityImpersonation, SetTokenInformation, TOKEN_ACCESS_MASK,
    TOKEN_ADJUST_DEFAULT, TOKEN_ADJUST_SESSIONID, TOKEN_ALL_ACCESS, TOKEN_ASSIGN_PRIMARY,
    TOKEN_DUPLICATE, TOKEN_QUERY, TokenPrimary, TokenSessionId,
};
use windows::Win32::System::RemoteDesktop::WTSGetActiveConsoleSessionId;
use windows::Win32::System::Threading::{
    CREATE_NO_WINDOW, CREATE_UNICODE_ENVIRONMENT, CreateProcessAsUserW, GetCurrentProcess,
    INFINITE, OpenProcessToken, PROCESS_INFORMATION, STARTUPINFOW, WaitForSingleObject,
};
use windows::core::{PCWSTR, PWSTR};

/// A running helper process.
pub struct Helper {
    process: HANDLE,
}

unsafe impl Send for Helper {}

impl Helper {
    pub fn wait(&mut self) -> Result<()> {
        unsafe {
            WaitForSingleObject(self.process, INFINITE);
        }
        Ok(())
    }
}

impl Drop for Helper {
    fn drop(&mut self) {
        unsafe {
            let _ = CloseHandle(self.process);
        }
    }
}

fn wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

pub fn start_helper(req: &SessionRequest, server: &ServerConfig) -> Result<Helper> {
    let session = unsafe { WTSGetActiveConsoleSessionId() };
    if session == u32::MAX {
        bail!("no console session");
    }
    let exe = std::env::current_exe()?;
    let cmd = format!(
        "\"{}\" session --request {} --server {}",
        exe.display(),
        crate::agent::encode_request(req),
        server.url.as_str().trim_end_matches('/')
    );
    unsafe {
        let mut tok = HANDLE::default();
        OpenProcessToken(
            GetCurrentProcess(),
            TOKEN_ACCESS_MASK(
                TOKEN_DUPLICATE.0
                    | TOKEN_QUERY.0
                    | TOKEN_ASSIGN_PRIMARY.0
                    | TOKEN_ADJUST_SESSIONID.0
                    | TOKEN_ADJUST_DEFAULT.0,
            ),
            &mut tok,
        )
        .context("OpenProcessToken")?;
        let mut dup = HANDLE::default();
        let r = DuplicateTokenEx(
            tok,
            TOKEN_ALL_ACCESS,
            None,
            SecurityImpersonation,
            TokenPrimary,
            &mut dup,
        );
        let _ = CloseHandle(tok);
        r.context("DuplicateTokenEx")?;
        let set = SetTokenInformation(dup, TokenSessionId, &session as *const u32 as *const _, 4);
        if let Err(e) = set {
            let _ = CloseHandle(dup);
            return Err(anyhow!("SetTokenInformation: {e}"));
        }
        let mut desktop = wide(r"winsta0\default");
        let si = STARTUPINFOW {
            cb: std::mem::size_of::<STARTUPINFOW>() as u32,
            lpDesktop: PWSTR(desktop.as_mut_ptr()),
            ..Default::default()
        };
        let mut pi = PROCESS_INFORMATION::default();
        let mut cmdline = wide(&cmd);
        let r = CreateProcessAsUserW(
            Some(dup),
            PCWSTR::null(),
            Some(PWSTR(cmdline.as_mut_ptr())),
            None,
            None,
            false,
            CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT,
            None,
            PCWSTR::null(),
            &si,
            &mut pi,
        );
        let _ = CloseHandle(dup);
        r.context("CreateProcessAsUserW")?;
        let _ = CloseHandle(pi.hThread);
        Ok(Helper {
            process: pi.hProcess,
        })
    }
}
