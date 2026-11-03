//! RHIS Remote agent.
//!
//! On Windows it runs as the `RHISRemoteAgent` service (LocalSystem) and keeps an outbound,
//! encrypted control channel to the server, so the machine is reachable behind NAT without
//! port forwarding. Each remote session runs in a helper process in the console session.
// SPDX-License-Identifier: AGPL-3.0-only
#![cfg_attr(not(debug_assertions), windows_subsystem = "console")]

mod agent;
mod buildcfg;
mod config;
mod secret;
mod sysinfo;
#[cfg(windows)]
mod win;

use std::path::PathBuf;

use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};

use crate::config::AgentConfig;

#[derive(Parser)]
#[command(
    name = "rhis-agent",
    version,
    about = "RHIS Remote agent: unattended remote access for this computer"
)]
struct Cli {
    /// Path of agent.json.
    #[arg(long, global = true)]
    config: Option<PathBuf>,
    #[command(subcommand)]
    cmd: Option<Cmd>,
}

#[derive(Subcommand)]
enum Cmd {
    /// Enroll this computer and install the agent as a Windows service.
    Install(EnrollArgs),
    /// Enroll without installing a service (development, or re-enroll an installed agent).
    Enroll(EnrollArgs),
    /// Stop and remove the service and its configuration.
    Uninstall,
    /// Run in the foreground (for troubleshooting).
    Run,
    /// Show the stored configuration.
    Status,
    /// Entry point for the Windows service manager.
    #[command(hide = true)]
    Service,
    /// Session helper started by the service in the console session.
    #[command(hide = true)]
    Session {
        #[arg(long)]
        request: String,
        #[arg(long)]
        server: String,
    },
}

#[derive(clap::Args, Clone)]
pub struct EnrollArgs {
    /// Server URL, for example https://remote.example.com (https only).
    #[arg(long)]
    server: Option<String>,
    /// Enrollment token from the portal (Devices, Add device).
    #[arg(long)]
    token: Option<String>,
    /// SHA-256 of the server certificate, for a server without a public certificate.
    #[arg(long)]
    pin: Option<String>,
    /// Development only: allow http:// to localhost.
    #[arg(long, hide = true)]
    insecure_dev: bool,
}

fn main() {
    let cli = Cli::parse();
    let path = cli.config.clone().unwrap_or_else(config::default_path);
    let service_mode = matches!(cli.cmd, Some(Cmd::Service) | Some(Cmd::Session { .. }));
    agent::init_logging(&path, service_mode);
    if let Err(e) = real_main(cli, path) {
        tracing::error!("{e:#}");
        eprintln!("error: {e:#}");
        std::process::exit(1);
    }
}

fn real_main(cli: Cli, path: PathBuf) -> Result<()> {
    match cli.cmd.unwrap_or(Cmd::Run) {
        Cmd::Install(a) => install(&path, a),
        Cmd::Enroll(a) => {
            let cfg = enroll(&path, a)?;
            println!(
                "Enrolled as device {} (ID {}).",
                cfg.device_id,
                format_code(&cfg.device_code)
            );
            Ok(())
        }
        Cmd::Uninstall => uninstall(&path),
        Cmd::Run => {
            let cfg = AgentConfig::load(&path)?
                .context("not enrolled yet: run `rhis-agent install --server … --token …`")?;
            agent::runtime().block_on(agent::run_foreground(cfg, path))
        }
        Cmd::Status => {
            match AgentConfig::load(&path)? {
                Some(c) => {
                    println!("Configuration: {}", path.display());
                    println!("Servers:       {}", c.servers.join(", "));
                    println!(
                        "Device:        {} (ID {})",
                        c.device_id,
                        format_code(&c.device_code)
                    );
                    println!("Machine ID:    {}", c.machine_id);
                    println!("Version:       {}", rhis_core::VERSION);
                }
                None => println!("Not enrolled ({} does not exist).", path.display()),
            }
            Ok(())
        }
        Cmd::Service => {
            #[cfg(windows)]
            {
                win::service::run(path)
            }
            #[cfg(not(windows))]
            {
                bail!("the service mode is only available on Windows")
            }
        }
        Cmd::Session { request, server } => agent::run_helper(&path, &request, &server),
    }
}

pub fn format_code(c: &str) -> String {
    if c.len() == 9 {
        format!("{} {} {}", &c[..3], &c[3..6], &c[6..])
    } else {
        c.to_string()
    }
}

/// Fills in server and token from the embedded build configuration when they are not given.
fn resolve_args(mut a: EnrollArgs) -> Result<(EnrollArgs, Option<buildcfg::BuildConfig>)> {
    let embedded = std::env::current_exe()
        .ok()
        .and_then(|p| buildcfg::read(&p).ok().flatten());
    if let Some(b) = &embedded {
        if a.server.is_none() {
            a.server = b.servers.first().cloned();
        }
        if a.token.is_none() && !b.enroll_token.is_empty() {
            a.token = Some(b.enroll_token.clone());
        }
    }
    if a.server.is_none() || a.token.is_none() {
        bail!(
            "--server and --token are required (or use a build made in the portal's client builder)"
        );
    }
    Ok((a, embedded))
}

fn enroll(path: &std::path::Path, a: EnrollArgs) -> Result<AgentConfig> {
    let (a, embedded) = resolve_args(a)?;
    let server_url = a.server.unwrap();
    let server = rhis_core::net::ServerConfig::new(&server_url, a.pin.as_deref(), a.insecure_dev)?;
    let mut cfg = AgentConfig::load(path)?.unwrap_or_default();
    if cfg.machine_id.is_empty() {
        cfg.machine_id = sysinfo::machine_id();
    }
    let info = sysinfo::collect();
    let req = rhis_core::control::EnrollRequest {
        enroll_token: a.token.as_deref().unwrap(),
        machine_id: &cfg.machine_id,
        hostname: &info.hostname,
        os_version: &info.os_version,
        agent_version: rhis_core::VERSION,
    };
    let res = agent::runtime().block_on(rhis_core::control::enroll(&server, &req))?;
    let mut servers = vec![server.url.as_str().trim_end_matches('/').to_string()];
    if let Some(b) = &embedded {
        for s in &b.servers {
            let s = s.trim_end_matches('/').to_string();
            if !servers.contains(&s) {
                servers.push(s);
            }
        }
    }
    cfg.servers = servers;
    cfg.server_cert_sha256 = a.pin.unwrap_or_default();
    cfg.device_id = res.device_id;
    cfg.device_code = res.device_code;
    cfg.set_token(&res.device_token)?;
    cfg.insecure_dev = a.insecure_dev;
    if embedded.is_some() {
        cfg.build = embedded;
    }
    cfg.save(path)?;
    Ok(cfg)
}

fn install(path: &std::path::Path, a: EnrollArgs) -> Result<()> {
    #[cfg(windows)]
    {
        win::install::require_admin()?;
        let cfg = enroll(path, a)?;
        win::install::install_service()?;
        println!(
            "Installed. This computer is device {} (ID {}).",
            cfg.device_id,
            format_code(&cfg.device_code)
        );
        Ok(())
    }
    #[cfg(not(windows))]
    {
        let cfg = enroll(path, a)?;
        println!(
            "Enrolled as device {} (ID {}). Run `rhis-agent run` to go online.",
            cfg.device_id,
            format_code(&cfg.device_code)
        );
        Ok(())
    }
}

fn uninstall(path: &std::path::Path) -> Result<()> {
    #[cfg(windows)]
    {
        win::install::require_admin()?;
        win::install::uninstall_service()?;
    }
    if path.exists() {
        std::fs::remove_file(path)?;
    }
    println!("Uninstalled.");
    Ok(())
}
