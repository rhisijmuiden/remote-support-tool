// SPDX-License-Identifier: AGPL-3.0-only
// Embeds the icon, version info and an asInvoker manifest in the Windows exe,
// so it runs without administrator rights and without installation.
fn main() {
    println!("cargo:rerun-if-env-changed=RHIS_SERVER_URL");
    #[cfg(windows)]
    {
        let mut res = winresource::WindowsResource::new();
        res.set_icon("assets/rhis-icon.ico");
        res.set("ProductName", "RHIS Remote QuickSupport");
        res.set("FileDescription", "RHIS Remote QuickSupport");
        res.set("CompanyName", "RHIS");
        res.set("LegalCopyright", "Copyright (C) 2026 RHIS. AGPL-3.0-only.");
        res.set_manifest(include_str!("assets/quicksupport.manifest"));
        res.compile().expect("compile Windows resources");
    }
}
