// Embeds the icon and version information in the Windows executable.
fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() != Ok("windows") {
        return;
    }
    let mut res = winresource::WindowsResource::new();
    res.set_icon("../../branding/logo/rhis-icon.ico")
        .set("ProductName", "RHIS Remote Agent")
        .set("FileDescription", "RHIS Remote Agent")
        .set("CompanyName", "RHIS")
        .set("LegalCopyright", "Copyright (C) 2026 RHIS. AGPL-3.0-only.");
    if let Err(e) = res.compile() {
        // A missing resource compiler only loses the icon; the agent still works.
        println!("cargo:warning=no Windows resources embedded: {e}");
    }
}
