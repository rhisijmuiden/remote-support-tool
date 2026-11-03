// SPDX-License-Identifier: AGPL-3.0-only
//! RHIS Remote QuickSupport: a portable exe the customer starts once. It shows
//! a session code, asks before anyone connects, and leaves nothing installed.
#![cfg_attr(all(windows, not(debug_assertions)), windows_subsystem = "windows")]

mod backend;
mod config;
mod i18n;
mod ui;

use config::{Config, ConfigError};
use i18n::Lang;
use std::sync::mpsc;

fn main() -> eframe::Result {
    let args: Vec<String> = std::env::args().collect();
    let loaded = Config::load(&args);
    let lang = Lang::detect(loaded.as_ref().ok().and_then(|c| c.lang.as_deref()));

    let icon = image::load_from_memory(include_bytes!("../assets/rhis-icon-192.png"))
        .map(|i| i.to_rgba8())
        .ok();
    let mut viewport = egui::ViewportBuilder::default()
        .with_title("RHIS Remote QuickSupport")
        .with_inner_size([440.0, 600.0])
        .with_min_inner_size([400.0, 540.0])
        .with_resizable(true);
    if let Some(i) = icon {
        viewport = viewport.with_icon(egui::IconData {
            width: i.width(),
            height: i.height(),
            rgba: i.into_raw(),
        });
    }
    let options = eframe::NativeOptions {
        viewport,
        ..Default::default()
    };

    let (ev_tx, ev_rx) = mpsc::channel();
    let (cmd_tx, cmd_rx) = mpsc::channel();
    let cfg = match loaded {
        Ok(c) => c,
        Err(e) => {
            // Show the problem in the window instead of failing silently.
            let key = match e {
                ConfigError::NoServer => "no_server",
                ConfigError::NotHttps(_) | ConfigError::BadUrl(_) => "not_https",
            };
            let _ = ev_tx.send(backend::Event::Fatal {
                key,
                detail: format!("{e:?}"),
            });
            Config {
                server: String::new(),
                alternates: vec![],
                logo_png: None,
                accent: None,
                product_name: "RHIS Remote".into(),
                company_name: "RHIS".into(),
                insecure_dev: false,
                demo: false,
                lang: None,
            }
        }
    };
    let worker_cfg = cfg.clone();
    let start_worker = !cfg.server.is_empty() || cfg.demo;

    eframe::run_native(
        "rhis-quicksupport",
        options,
        Box::new(move |cc| {
            let ctx = cc.egui_ctx.clone();
            if start_worker {
                std::thread::spawn(move || {
                    backend::run(worker_cfg, lang, ev_tx, cmd_rx, move || {
                        ctx.request_repaint()
                    })
                });
            }
            Ok(Box::new(ui::App::new(cc, cfg, lang, ev_rx, cmd_tx)))
        }),
    )
}
