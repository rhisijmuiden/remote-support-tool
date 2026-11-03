// SPDX-License-Identifier: AGPL-3.0-only
//! The QuickSupport window, in the RHIS house style.

use crate::backend::{self, ANSWER_SECS, Cmd, Event, Latest, VERSION};
use crate::config::Config;
use crate::i18n::{Lang, t, tf};
use egui::{
    Align, Color32, CornerRadius, FontData, FontDefinitions, FontFamily, FontId, Frame, Layout,
    Margin, RichText, Stroke, TextureHandle, Theme, Vec2, ViewportCommand, WindowLevel,
};
use std::sync::mpsc::{Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

const NACHT: Color32 = Color32::from_rgb(0x14, 0x21, 0x3D);
const KORAAL: Color32 = Color32::from_rgb(0xFF, 0x6B, 0x57);

/// The accent color: koraal, or the one a client build sets.
static ACCENT: std::sync::OnceLock<Color32> = std::sync::OnceLock::new();

fn accent() -> Color32 {
    ACCENT.get().copied().unwrap_or(KORAAL)
}

/// Text on the accent color: nacht on light accents (koraal included, per the
/// house style), white on dark ones.
fn on_accent() -> Color32 {
    let c = accent();
    let lum = 0.2126 * c.r() as f32 + 0.7152 * c.g() as f32 + 0.0722 * c.b() as f32;
    if lum > 120.0 { NACHT } else { Color32::WHITE }
}
const LUCHT: Color32 = Color32::from_rgb(0x9C, 0xC3, 0xFF);

/// Colors from branding/tokens/tokens.css, light and dark.
#[derive(Clone, Copy)]
struct Palette {
    surface: Color32,
    raised: Color32,
    ink: Color32,
    muted: Color32,
    line: Color32,
    accent_text: Color32,
    online: Color32,
    warning: Color32,
    offline: Color32,
}

impl Palette {
    fn get(dark: bool) -> Palette {
        let c = Color32::from_rgb;
        if dark {
            Palette {
                surface: c(0x0E, 0x16, 0x26),
                raised: c(0x16, 0x21, 0x3A),
                ink: c(0xEE, 0xF1, 0xF7),
                muted: c(0xA3, 0xAE, 0xC4),
                line: c(0x26, 0x32, 0x4D),
                accent_text: c(0xFF, 0x8A, 0x78),
                online: c(0x4A, 0xCB, 0x8C),
                warning: c(0xF2, 0xB6, 0x40),
                offline: c(0xFF, 0x7A, 0x7A),
            }
        } else {
            Palette {
                surface: c(0xF6, 0xF7, 0xFB),
                raised: Color32::WHITE,
                ink: NACHT,
                muted: c(0x4F, 0x5B, 0x73),
                line: c(0xDC, 0xE1, 0xEB),
                accent_text: c(0xB8, 0x39, 0x2A),
                online: c(0x1C, 0x8A, 0x57),
                warning: c(0xA8, 0x6B, 0x00),
                offline: c(0xC2, 0x34, 0x34),
            }
        }
    }
}

enum Screen {
    Connecting,
    Offline {
        retry_in: u64,
    },
    Ready,
    Request {
        session_id: String,
        technician: String,
        recording: bool,
        since: Instant,
    },
    Active {
        technician: String,
        recording: bool,
    },
    Fatal {
        key: &'static str,
        detail: String,
    },
}

#[derive(Default)]
enum UpdateState {
    #[default]
    Idle,
    Checking,
    UpToDate,
    Available(Latest),
    Downloading,
    Failed(String),
}

pub struct App {
    cfg: Config,
    lang: Lang,
    events: Receiver<Event>,
    cmds: Sender<Cmd>,
    screen: Screen,
    code: Option<String>,
    notice: Option<(&'static str, Instant)>,
    copied: Option<Instant>,
    logo: TextureHandle,
    info_open: bool,
    update: Arc<Mutex<UpdateState>>,
    on_top: bool,
}

pub fn install_fonts(ctx: &egui::Context) {
    let mut fonts = FontDefinitions::default();
    let add = |fonts: &mut FontDefinitions, name: &str, bytes: &'static [u8]| {
        fonts
            .font_data
            .insert(name.into(), Arc::new(FontData::from_static(bytes)));
    };
    add(
        &mut fonts,
        "archivo",
        include_bytes!("../assets/Archivo-Regular.ttf"),
    );
    add(
        &mut fonts,
        "archivo-semibold",
        include_bytes!("../assets/Archivo-SemiBold.ttf"),
    );
    add(
        &mut fonts,
        "archivo-display",
        include_bytes!("../assets/Archivo-ExpandedExtraBold.ttf"),
    );
    // Archivo first, egui's own fonts after it as fallback for symbols.
    let fallback = fonts
        .families
        .get(&FontFamily::Proportional)
        .cloned()
        .unwrap_or_default();
    fonts.families.insert(
        FontFamily::Proportional,
        [vec!["archivo".into()], fallback.clone()].concat(),
    );
    for (fam, font) in [
        ("semibold", "archivo-semibold"),
        ("display", "archivo-display"),
    ] {
        fonts.families.insert(
            FontFamily::Name(fam.into()),
            [vec![font.into()], fallback.clone()].concat(),
        );
    }
    ctx.set_fonts(fonts);
}

fn display(size: f32) -> FontId {
    FontId::new(size, FontFamily::Name("display".into()))
}
fn semibold(size: f32) -> FontId {
    FontId::new(size, FontFamily::Name("semibold".into()))
}

fn try_load_png(ctx: &egui::Context, name: &str, bytes: &[u8]) -> Option<TextureHandle> {
    let img = image::load_from_memory_with_format(bytes, image::ImageFormat::Png)
        .ok()?
        .to_rgba8();
    let size = [img.width() as usize, img.height() as usize];
    Some(ctx.load_texture(
        name,
        egui::ColorImage::from_rgba_unmultiplied(size, img.as_raw()),
        Default::default(),
    ))
}

fn load_png(ctx: &egui::Context, name: &str, bytes: &[u8]) -> TextureHandle {
    let img = image::load_from_memory_with_format(bytes, image::ImageFormat::Png)
        .expect("bundled png")
        .to_rgba8();
    let size = [img.width() as usize, img.height() as usize];
    ctx.load_texture(
        name,
        egui::ColorImage::from_rgba_unmultiplied(size, img.as_raw()),
        Default::default(),
    )
}

impl App {
    pub fn new(
        cc: &eframe::CreationContext<'_>,
        cfg: Config,
        lang: Lang,
        events: Receiver<Event>,
        cmds: Sender<Cmd>,
    ) -> Self {
        install_fonts(&cc.egui_ctx);
        // Follows the Windows light/dark setting; RHIS_QS_THEME forces one (docs screenshots).
        match std::env::var("RHIS_QS_THEME").as_deref() {
            Ok("light") => cc.egui_ctx.set_theme(egui::ThemePreference::Light),
            Ok("dark") => cc.egui_ctx.set_theme(egui::ThemePreference::Dark),
            _ => {}
        }
        if let Some([r, g, b]) = cfg.accent {
            let _ = ACCENT.set(Color32::from_rgb(r, g, b));
        }
        // A client build can bring its own logo; otherwise the RHIS logo.
        let logo = cfg
            .logo_png
            .as_deref()
            .and_then(|png| try_load_png(&cc.egui_ctx, "logo", png))
            .unwrap_or_else(|| {
                load_png(
                    &cc.egui_ctx,
                    "logo",
                    include_bytes!("../assets/rhis-logo-wit-600.png"),
                )
            });
        App {
            cfg,
            lang,
            events,
            cmds,
            screen: Screen::Connecting,
            code: None,
            notice: None,
            copied: None,
            logo,
            info_open: false,
            update: Default::default(),
            on_top: false,
        }
    }

    fn handle(&mut self, ctx: &egui::Context, e: Event) {
        match e {
            Event::Connecting => self.screen = Screen::Connecting,
            Event::Offline { retry_in } => self.screen = Screen::Offline { retry_in },
            Event::Ready { code } => {
                self.code = Some(code);
                self.screen = Screen::Ready;
            }
            Event::Request {
                session_id,
                technician,
                recording,
            } => {
                self.screen = Screen::Request {
                    session_id,
                    technician,
                    recording,
                    since: Instant::now(),
                };
                // Make sure the customer sees the question.
                ctx.send_viewport_cmd(ViewportCommand::Minimized(false));
                ctx.send_viewport_cmd(ViewportCommand::WindowLevel(WindowLevel::AlwaysOnTop));
                ctx.send_viewport_cmd(ViewportCommand::Focus);
                ctx.send_viewport_cmd(ViewportCommand::RequestUserAttention(
                    egui::UserAttentionType::Critical,
                ));
                self.on_top = true;
            }
            Event::RequestCancelled => {
                self.screen = Screen::Ready;
                self.notice = Some(("cancelled", Instant::now()));
            }
            Event::Active {
                technician,
                recording,
            } => {
                self.screen = Screen::Active {
                    technician,
                    recording,
                }
            }
            Event::Ended { declined } => {
                self.screen = Screen::Ready;
                self.notice = Some((if declined { "declined" } else { "ended" }, Instant::now()));
            }
            Event::Fatal { key, detail } => self.screen = Screen::Fatal { key, detail },
        }
        if self.on_top && !matches!(self.screen, Screen::Request { .. }) {
            ctx.send_viewport_cmd(ViewportCommand::WindowLevel(WindowLevel::Normal));
            self.on_top = false;
        }
    }

    fn answer(&mut self, allow: bool) {
        if let Screen::Request { session_id, .. } = &self.screen {
            let _ = self.cmds.send(Cmd::Answer {
                session_id: session_id.clone(),
                allow,
            });
        }
    }
}

fn primary(ui: &mut egui::Ui, text: &str, width: f32) -> egui::Response {
    ui.add(
        egui::Button::new(RichText::new(text).font(semibold(16.0)).color(on_accent()))
            .fill(accent())
            .stroke(Stroke::NONE)
            .corner_radius(CornerRadius::same(10))
            .min_size(Vec2::new(width, 44.0)),
    )
}

fn secondary(ui: &mut egui::Ui, p: Palette, text: &str, width: f32) -> egui::Response {
    ui.add(
        egui::Button::new(RichText::new(text).font(semibold(16.0)).color(p.ink))
            .fill(p.raised)
            .stroke(Stroke::new(1.0, p.line))
            .corner_radius(CornerRadius::same(10))
            .min_size(Vec2::new(width, 44.0)),
    )
}

fn card(p: Palette) -> Frame {
    Frame::new()
        .fill(p.raised)
        .stroke(Stroke::new(1.0, p.line))
        .corner_radius(CornerRadius::same(16))
        .inner_margin(Margin::same(20))
}

/// A small padlock, drawn so it does not depend on a symbol font.
fn lock_icon(ui: &mut egui::Ui, color: Color32) {
    let (r, _) = ui.allocate_exact_size(Vec2::new(10.0, 13.0), egui::Sense::hover());
    let body = egui::Rect::from_min_max(egui::pos2(r.left(), r.top() + 5.5), r.right_bottom());
    ui.painter().rect_filled(body, CornerRadius::same(2), color);
    let c = egui::pos2(r.center().x, r.top() + 5.5);
    let pts: Vec<egui::Pos2> = (0..=12)
        .map(|i| {
            let a = std::f32::consts::PI * (i as f32 / 12.0);
            egui::pos2(c.x - 3.2 * a.cos(), c.y - 4.5 * a.sin())
        })
        .collect();
    ui.painter()
        .add(egui::Shape::line(pts, Stroke::new(1.6, color)));
}

fn status_dot(ui: &mut egui::Ui, color: Color32, text: &str, p: Palette) {
    ui.horizontal(|ui| {
        let (r, _) = ui.allocate_exact_size(Vec2::splat(10.0), egui::Sense::hover());
        ui.painter().circle_filled(r.center(), 5.0, color);
        ui.label(RichText::new(text).color(p.ink).size(14.0));
    });
}

impl eframe::App for App {
    fn logic(&mut self, ctx: &egui::Context, _frame: &mut eframe::Frame) {
        while let Ok(e) = self.events.try_recv() {
            self.handle(ctx, e);
        }
        if let Screen::Request { since, .. } = &self.screen {
            if since.elapsed() >= Duration::from_secs(ANSWER_SECS) {
                self.answer(false);
            }
            ctx.request_repaint_after(Duration::from_millis(250));
        }
        if self
            .notice
            .is_some_and(|(_, at)| at.elapsed() > Duration::from_secs(8))
        {
            self.notice = None;
        }
        if ctx.input(|i| i.viewport().close_requested()) {
            let _ = self.cmds.send(Cmd::Shutdown);
        }
    }

    fn ui(&mut self, ui: &mut egui::Ui, _frame: &mut eframe::Frame) {
        let ctx = ui.ctx().clone();
        let dark = ctx.theme() == Theme::Dark;
        let p = Palette::get(dark);
        let lang = self.lang;

        egui::Panel::top("header")
            .frame(
                Frame::new()
                    .fill(NACHT)
                    .inner_margin(Margin::symmetric(24, 18)),
            )
            .show(ui, |ui| {
                ui.horizontal(|ui| {
                    ui.add(egui::Image::new(&self.logo).fit_to_exact_size(Vec2::new(160.0, 32.0)));
                    ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                        ui.label(
                            RichText::new(t(lang, "title"))
                                .font(display(15.0))
                                .color(LUCHT),
                        );
                    });
                });
            });

        egui::Panel::bottom("footer")
            .frame(
                Frame::new()
                    .fill(p.raised)
                    .stroke(Stroke::new(1.0, p.line))
                    .inner_margin(Margin::symmetric(16, 10)),
            )
            .show(ui, |ui| {
                ui.horizontal(|ui| {
                    if ui
                        .add(
                            egui::Button::new(RichText::new(t(lang, "info")).color(p.ink))
                                .frame(false),
                        )
                        .clicked()
                    {
                        self.info_open = true;
                    }
                    ui.label(
                        RichText::new(format!("v{VERSION}"))
                            .color(p.muted)
                            .size(12.0),
                    );
                    ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                        for (l, label) in [(Lang::Nl, "NL"), (Lang::En, "EN")] {
                            let on = self.lang == l;
                            let b =
                                egui::Button::new(RichText::new(label).size(12.0).color(if on {
                                    NACHT
                                } else {
                                    p.muted
                                }))
                                .fill(if on { LUCHT } else { Color32::TRANSPARENT })
                                .stroke(Stroke::new(1.0, p.line))
                                .corner_radius(CornerRadius::same(6));
                            if ui.add(b).clicked() {
                                self.lang = l;
                            }
                        }
                        ui.add_space(8.0);
                        if self.cfg.server.starts_with("https://") || self.cfg.demo {
                            ui.label(
                                RichText::new(t(lang, "encrypted"))
                                    .color(p.online)
                                    .size(12.0),
                            );
                            lock_icon(ui, p.online);
                        } else if !self.cfg.server.is_empty() {
                            ui.label(
                                RichText::new(t(lang, "unencrypted_dev"))
                                    .color(p.offline)
                                    .size(12.0),
                            );
                        }
                    });
                });
            });

        egui::CentralPanel::default()
            .frame(Frame::new().fill(p.surface).inner_margin(Margin::same(24)))
            .show(ui, |ui| self.body(ui, p));

        if self.info_open {
            self.info(&ctx, p);
        }
    }
}

impl App {
    fn body(&mut self, ui: &mut egui::Ui, p: Palette) {
        let lang = self.lang;
        let w = ui.available_width();
        if self.cfg.demo && std::env::var_os("RHIS_QS_HIDE_DEMO_BANNER").is_none() {
            ui.label(RichText::new(t(lang, "demo")).color(p.warning).size(12.0));
            ui.add_space(6.0);
        }
        match &self.screen {
            Screen::Connecting => {
                ui.add_space(80.0);
                ui.vertical_centered(|ui| {
                    ui.add(egui::Spinner::new().size(36.0).color(accent()));
                    ui.add_space(16.0);
                    ui.label(RichText::new(t(lang, "connecting")).color(p.ink).size(16.0));
                    ui.label(RichText::new(&self.cfg.server).color(p.muted).size(12.0));
                });
            }
            Screen::Offline { retry_in } => {
                ui.add_space(60.0);
                let retry_in = *retry_in;
                card(p).show(ui, |ui| {
                    ui.set_width(ui.available_width());
                    status_dot(
                        ui,
                        p.warning,
                        &tf(lang, "offline", &[&retry_in.to_string()]),
                        p,
                    );
                    ui.add_space(12.0);
                    if secondary(ui, p, t(lang, "retry"), 160.0).clicked() {
                        let _ = self.cmds.send(Cmd::Retry);
                    }
                });
            }
            Screen::Ready => self.ready(ui, p, w),
            Screen::Request {
                technician,
                recording,
                since,
                ..
            } => {
                let (technician, recording) = (technician.clone(), *recording);
                let left = ANSWER_SECS.saturating_sub(since.elapsed().as_secs());
                Frame::new()
                    .fill(p.raised)
                    .stroke(Stroke::new(2.0, accent()))
                    .corner_radius(CornerRadius::same(16))
                    .inner_margin(Margin::same(22))
                    .show(ui, |ui| {
                        ui.set_width(ui.available_width());
                        ui.label(
                            RichText::new(t(lang, "request_title"))
                                .font(display(22.0))
                                .color(p.ink),
                        );
                        ui.add_space(10.0);
                        ui.label(
                            RichText::new(tf(
                                lang,
                                "request_body",
                                &[&technician, &self.cfg.company_name],
                            ))
                            .color(p.ink)
                            .size(16.0),
                        );
                        if recording {
                            ui.add_space(8.0);
                            status_dot(ui, p.offline, t(lang, "request_rec"), p);
                        }
                        ui.add_space(18.0);
                        let bw = (ui.available_width() - 12.0) / 2.0;
                        ui.horizontal(|ui| {
                            if primary(ui, t(lang, "allow"), bw).clicked() {
                                self.answer(true);
                            }
                            ui.add_space(4.0);
                            if secondary(ui, p, t(lang, "decline"), bw).clicked() {
                                self.answer(false);
                            }
                        });
                        ui.add_space(10.0);
                        ui.label(
                            RichText::new(tf(lang, "answer_in", &[&left.to_string()]))
                                .color(p.muted)
                                .size(12.0),
                        );
                    });
            }
            Screen::Active {
                technician,
                recording,
            } => {
                let (technician, recording) = (technician.clone(), *recording);
                Frame::new()
                    .fill(accent())
                    .corner_radius(CornerRadius::same(16))
                    .inner_margin(Margin::same(22))
                    .show(ui, |ui| {
                        ui.set_width(ui.available_width());
                        ui.label(
                            RichText::new(t(lang, "active_title"))
                                .font(display(20.0))
                                .color(on_accent()),
                        );
                        ui.add_space(6.0);
                        ui.label(
                            RichText::new(tf(lang, "active_body", &[&technician]))
                                .color(on_accent())
                                .size(16.0),
                        );
                        if recording {
                            ui.add_space(10.0);
                            Frame::new()
                                .fill(NACHT)
                                .corner_radius(CornerRadius::same(6))
                                .inner_margin(Margin::symmetric(8, 3))
                                .show(ui, |ui| {
                                    status_dot(
                                        ui,
                                        accent(),
                                        t(lang, "recording"),
                                        Palette {
                                            ink: Color32::WHITE,
                                            ..p
                                        },
                                    );
                                });
                        }
                    });
                ui.add_space(20.0);
                let r = ui.vertical_centered_justified(|ui| {
                    ui.add(
                        egui::Button::new(
                            RichText::new(t(lang, "end_session"))
                                .font(semibold(16.0))
                                .color(Color32::WHITE),
                        )
                        .fill(NACHT)
                        .corner_radius(CornerRadius::same(10))
                        .min_size(Vec2::new(w, 48.0)),
                    )
                });
                if r.inner.clicked() {
                    let _ = self.cmds.send(Cmd::EndSession);
                }
                ui.add_space(16.0);
                self.code_line(ui, p);
            }
            Screen::Fatal { key, detail } => {
                let (key, detail) = (*key, detail.clone());
                ui.add_space(40.0);
                card(p).show(ui, |ui| {
                    ui.set_width(ui.available_width());
                    ui.label(
                        RichText::new(t(lang, "error"))
                            .font(display(20.0))
                            .color(p.offline),
                    );
                    ui.add_space(8.0);
                    ui.label(RichText::new(t(lang, key)).color(p.ink).size(15.0));
                    if !detail.is_empty() {
                        ui.add_space(6.0);
                        ui.label(RichText::new(detail).color(p.muted).size(12.0));
                    }
                    if key == "engine_missing" {
                        ui.add_space(12.0);
                        self.code_line(ui, p);
                    }
                });
            }
        }
    }

    fn ready(&mut self, ui: &mut egui::Ui, p: Palette, w: f32) {
        let lang = self.lang;
        let code = self.code.clone().unwrap_or_default();
        card(p).show(ui, |ui| {
            ui.set_width(ui.available_width());
            ui.label(
                RichText::new(t(lang, "your_code").to_uppercase())
                    .color(p.muted)
                    .font(semibold(12.0)),
            );
            ui.add_space(6.0);
            ui.horizontal(|ui| {
                ui.label(
                    RichText::new(backend::format_code(&code))
                        .font(display(38.0))
                        .color(p.ink),
                );
            });
            ui.add_space(12.0);
            let copied = self
                .copied
                .is_some_and(|c| c.elapsed() < Duration::from_secs(2));
            let label = if copied {
                t(lang, "copied")
            } else {
                t(lang, "copy")
            };
            if secondary(ui, p, label, 140.0).clicked() {
                ui.ctx().copy_text(code.clone());
                self.copied = Some(Instant::now());
            }
            if copied {
                ui.ctx().request_repaint_after(Duration::from_millis(500));
            }
        });
        ui.add_space(16.0);
        status_dot(ui, p.online, t(lang, "ready"), p);
        ui.add_space(10.0);
        ui.label(
            RichText::new(tf(lang, "give_code", &[&self.cfg.company_name]))
                .color(p.ink)
                .size(15.0),
        );
        ui.add_space(6.0);
        ui.label(RichText::new(t(lang, "privacy")).color(p.muted).size(14.0));
        if let Some((key, _)) = self.notice {
            ui.add_space(16.0);
            Frame::new()
                .fill(p.raised)
                .stroke(Stroke::new(1.0, p.line))
                .corner_radius(CornerRadius::same(10))
                .inner_margin(Margin::symmetric(14, 10))
                .show(ui, |ui| {
                    ui.set_width(w - 28.0);
                    ui.label(RichText::new(t(lang, key)).color(p.ink).size(14.0));
                });
            ui.ctx().request_repaint_after(Duration::from_secs(1));
        }
        ui.with_layout(Layout::bottom_up(Align::Min), |ui| {
            ui.label(
                RichText::new(t(lang, "close_hint"))
                    .color(p.muted)
                    .size(12.0),
            );
        });
    }

    fn code_line(&self, ui: &mut egui::Ui, p: Palette) {
        if let Some(code) = &self.code {
            ui.label(
                RichText::new(format!(
                    "{}: {}",
                    t(self.lang, "your_code"),
                    backend::format_code(code)
                ))
                .color(p.muted)
                .size(13.0),
            );
        }
    }

    fn info(&mut self, ctx: &egui::Context, p: Palette) {
        let lang = self.lang;
        let modal = egui::Modal::new(egui::Id::unique("info"))
            .frame(
                Frame::new()
                    .fill(p.raised)
                    .corner_radius(CornerRadius::same(16))
                    .inner_margin(Margin::same(22)),
            )
            .show(ctx, |ui| {
                ui.set_width(320.0);
                ui.label(
                    RichText::new(format!("{} QuickSupport", self.cfg.product_name))
                        .font(display(18.0))
                        .color(p.ink),
                );
                ui.add_space(12.0);
                egui::Grid::new("info-grid")
                    .num_columns(2)
                    .spacing([16.0, 6.0])
                    .show(ui, |ui| {
                        let row = |ui: &mut egui::Ui, k: &str, v: &str| {
                            ui.label(RichText::new(k).color(p.muted));
                            ui.label(RichText::new(v).color(p.ink));
                            ui.end_row();
                        };
                        row(ui, t(lang, "version"), VERSION);
                        row(
                            ui,
                            t(lang, "server"),
                            if self.cfg.demo {
                                "demo"
                            } else {
                                &self.cfg.server
                            },
                        );
                        row(ui, t(lang, "license"), "AGPL-3.0-only");
                    });
                ui.add_space(4.0);
                ui.hyperlink_to(
                    RichText::new(t(lang, "source")).color(p.accent_text),
                    "https://github.com/rhisijmuiden/remote-support-tool",
                );
                ui.add_space(14.0);
                let state = std::mem::take(&mut *self.update.lock().unwrap());
                let mut next = None;
                match &state {
                    UpdateState::Idle => {}
                    UpdateState::Checking => {
                        ui.horizontal(|ui| {
                            ui.add(egui::Spinner::new().color(accent()));
                            ui.label(RichText::new(t(lang, "checking")).color(p.ink));
                        });
                    }
                    UpdateState::Downloading => {
                        ui.horizontal(|ui| {
                            ui.add(egui::Spinner::new().color(accent()));
                            ui.label(RichText::new(t(lang, "downloading")).color(p.ink));
                        });
                    }
                    UpdateState::UpToDate => {
                        status_dot(ui, p.online, t(lang, "up_to_date"), p);
                    }
                    UpdateState::Failed(e) => {
                        ui.label(RichText::new(tf(lang, "update_failed", &[e])).color(p.offline));
                    }
                    UpdateState::Available(l) => {
                        status_dot(
                            ui,
                            p.warning,
                            &tf(lang, "update_available", &[&l.version]),
                            p,
                        );
                        ui.add_space(8.0);
                        if primary(ui, t(lang, "download_update"), 220.0).clicked() {
                            next = Some(UpdateState::Downloading);
                            let (cfg, l, slot, ctx2) = (
                                self.cfg.clone(),
                                l.clone(),
                                self.update.clone(),
                                ctx.clone(),
                            );
                            std::thread::spawn(move || {
                                let r = backend::download_and_start(&cfg, &l);
                                match r {
                                    Ok(()) => ctx2.send_viewport_cmd(ViewportCommand::Close),
                                    Err(e) => *slot.lock().unwrap() = UpdateState::Failed(e),
                                }
                                ctx2.request_repaint();
                            });
                        }
                    }
                }
                let busy = matches!(state, UpdateState::Checking | UpdateState::Downloading);
                *self.update.lock().unwrap() = next.unwrap_or(state);
                ui.add_space(14.0);
                ui.horizontal(|ui| {
                    if !busy && secondary(ui, p, t(lang, "check_updates"), 190.0).clicked() {
                        *self.update.lock().unwrap() = UpdateState::Checking;
                        let (cfg, slot, ctx2) =
                            (self.cfg.clone(), self.update.clone(), ctx.clone());
                        std::thread::spawn(move || {
                            let s = if cfg.demo {
                                std::thread::sleep(Duration::from_millis(600));
                                UpdateState::UpToDate
                            } else {
                                match backend::check_latest(&cfg) {
                                    Ok(l) if backend::is_newer(&l.version, VERSION) => {
                                        UpdateState::Available(l)
                                    }
                                    Ok(_) => UpdateState::UpToDate,
                                    Err(e) => UpdateState::Failed(e),
                                }
                            };
                            *slot.lock().unwrap() = s;
                            ctx2.request_repaint();
                        });
                    }
                    if secondary(ui, p, t(lang, "close"), 100.0).clicked() {
                        ui.close();
                    }
                });
            });
        if modal.should_close() {
            self.info_open = false;
        }
    }
}
