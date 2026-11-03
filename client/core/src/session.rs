//! One remote session: capture the screen, encode H.264, send it over WebRTC (or the
//! WebSocket relay when WebRTC cannot connect) and apply the technician's input.
use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};
use std::sync::{Arc, mpsc as stdmpsc};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, anyhow};
use futures_util::{SinkExt, StreamExt};
use rtc::ice::mdns::MulticastDnsMode;
use rtc::media::Sample;
use rtc::media_stream::MediaStreamTrack;
use rtc::peer_connection::sdp::RTCSessionDescription;
use rtc::rtp_transceiver::PayloadType;
use rtc::rtp_transceiver::rtp_sender::{
    RTCPFeedback, RTCRtpCodec, RTCRtpCodecParameters, RTCRtpCodingParameters, RTCRtpEncodingParameters, RtpCodecKind,
};
use tokio::sync::{Mutex, mpsc};
use tokio_tungstenite::tungstenite::Message;
use tracing::{debug, info, warn};
use webrtc::data_channel::{DataChannel, DataChannelEvent};
use webrtc::media_stream::track_local::TrackLocal;
use webrtc::media_stream::track_local::static_sample::TrackLocalStaticSample;
use webrtc::peer_connection::{
    MediaEngine, PeerConnection, PeerConnectionBuilder, PeerConnectionEventHandler, RTCConfigurationBuilder,
    RTCIceCandidateInit, RTCIceServer, RTCPeerConnectionIceEvent, RTCPeerConnectionState, Registry,
    SettingEngineBuilder, register_default_interceptors,
};
use webrtc::rtp_transceiver::RtpSender;

use crate::capture::Frame;
use crate::encode::{H264Encoder, default_bitrate};
use crate::net::ServerConfig;
use crate::platform::Platform;
use crate::protocol::{InputEvent, SessionRequest, Signal, relay_frame};

const MODE_NONE: u8 = 0;
const MODE_WEBRTC: u8 = 1;
const MODE_RELAY: u8 = 2;

/// Everything needed to run a session that the server asked for.
#[derive(Clone)]
pub struct SessionConfig {
    pub server: ServerConfig,
    pub token: String,
    pub request: SessionRequest,
    /// Frames per second to aim for.
    pub fps: f32,
    /// Product name for the on-screen banner.
    pub product_name: String,
}

struct EncodedFrame {
    data: Vec<u8>,
    key: bool,
    at: Instant,
}

enum PcEvent {
    Ice(RTCIceCandidateInit),
    State(RTCPeerConnectionState),
    Input(InputEvent),
}

struct Handler {
    tx: mpsc::UnboundedSender<PcEvent>,
}

#[async_trait::async_trait]
impl PeerConnectionEventHandler for Handler {
    async fn on_ice_candidate(&self, ev: RTCPeerConnectionIceEvent) {
        if let Ok(init) = ev.candidate.to_json() {
            let _ = self.tx.send(PcEvent::Ice(init));
        }
    }

    async fn on_connection_state_change(&self, state: RTCPeerConnectionState) {
        let _ = self.tx.send(PcEvent::State(state));
    }

    async fn on_data_channel(&self, dc: Arc<dyn DataChannel>) {
        let tx = self.tx.clone();
        tokio::spawn(async move {
            while let Some(ev) = dc.poll().await {
                if let DataChannelEvent::OnMessage(msg) = ev {
                    match serde_json::from_slice::<InputEvent>(&msg.data) {
                        Ok(ev) => {
                            let _ = tx.send(PcEvent::Input(ev));
                        }
                        Err(e) => debug!("bad input event: {e}"),
                    }
                }
            }
        });
    }
}

struct VideoOut {
    track: Arc<TrackLocalStaticSample>,
    ssrc: u32,
    pt: PayloadType,
}

/// Runs one session until it ends and returns the reason.
pub async fn run(cfg: SessionConfig, platform: Arc<dyn Platform>) -> Result<String> {
    let path = format!("/api/agent/v1/sessions/{}", cfg.request.session_id);
    let ws = cfg.server.connect_ws(&path, &cfg.token).await.context("session socket")?;
    let (mut sink, mut stream) = ws.split();
    let (out_tx, mut out_rx) = mpsc::channel::<Message>(64);
    let writer = tokio::spawn(async move {
        while let Some(m) = out_rx.recv().await {
            if sink.send(m).await.is_err() {
                break;
            }
        }
        let _ = sink.close().await;
    });
    let send = |s: Signal| {
        let tx = out_tx.clone();
        async move {
            let _ = tx.send(Message::Text(serde_json::to_string(&s).unwrap().into())).await;
        }
    };

    let mut banner_text = format!("{}: {}", cfg.product_name, cfg.request.user_name);
    if cfg.request.recording {
        banner_text.push_str(" · ● REC");
    }
    let _banner = platform.session_banner(&banner_text);

    let mode = Arc::new(AtomicU8::new(MODE_NONE));
    let key = Arc::new(AtomicBool::new(true));
    let stop = Arc::new(AtomicBool::new(false));
    let (input_tx, input_rx) = stdmpsc::channel::<InputEvent>();
    let mut input_rx = Some(input_rx);
    let (frames_tx, mut frames_rx) = mpsc::channel::<EncodedFrame>(3);
    let (screen_tx, mut screen_rx) = mpsc::unbounded_channel::<Signal>();
    let (pc_tx, mut pc_rx) = mpsc::unbounded_channel::<PcEvent>();
    let video: Arc<Mutex<Option<VideoOut>>> = Arc::new(Mutex::new(None));
    let mut pc: Option<Box<dyn PeerConnection>> = None;
    let mut media: Option<std::thread::JoinHandle<()>> = None;

    // Frame pump: sends encoded frames on whichever path is active.
    let pump = {
        let (mode, key, video, out_tx) = (mode.clone(), key.clone(), video.clone(), out_tx.clone());
        tokio::spawn(async move {
            let start = Instant::now();
            let mut last: Option<Instant> = None;
            while let Some(f) = frames_rx.recv().await {
                match mode.load(Ordering::Relaxed) {
                    MODE_WEBRTC => {
                        let v = video.lock().await;
                        if let Some(v) = v.as_ref() {
                            let duration = last.map(|l| f.at.saturating_duration_since(l)).unwrap_or(Duration::from_millis(33));
                            last = Some(f.at);
                            let sample = Sample { data: f.data.into(), duration, ..Sample::new(f.at) };
                            if let Err(e) = v.track.sample_writer(v.ssrc, v.pt).write_sample(&sample).await {
                                debug!("write sample: {e}");
                            }
                        }
                    }
                    MODE_RELAY => {
                        let ts = f.at.saturating_duration_since(start).as_micros() as u64;
                        let msg = Message::Binary(relay_frame(f.key, ts, &f.data).into());
                        if out_tx.try_send(msg).is_err() {
                            // The socket is behind: skip to the next key frame.
                            key.store(true, Ordering::Relaxed);
                        }
                    }
                    _ => {}
                }
            }
        })
    };

    let reason = loop {
        tokio::select! {
            msg = stream.next() => {
                let text = match msg {
                    Some(Ok(Message::Text(t))) => t,
                    Some(Ok(Message::Close(_))) | None => break "server_closed".to_string(),
                    Some(Ok(_)) => continue,
                    Some(Err(e)) => break format!("socket_error: {e}"),
                };
                let sig: Signal = match serde_json::from_str(&text) {
                    Ok(s) => s,
                    Err(e) => { debug!("bad signal: {e}"); continue; }
                };
                match sig {
                    Signal::Ready { .. } => {
                        if let Some(rx) = input_rx.take() {
                            media = Some(start_media(platform.clone(), cfg.fps, rx, key.clone(), stop.clone(), frames_tx.clone(), screen_tx.clone()));
                        }
                    }
                    Signal::Offer { sdp } => {
                        if let Some(old) = pc.take() { let _ = old.close().await; }
                        match answer(&cfg, &sdp, pc_tx.clone()).await {
                            Ok((p, v, answer_sdp)) => {
                                *video.lock().await = Some(v);
                                pc = Some(p);
                                mode.store(MODE_WEBRTC, Ordering::Relaxed);
                                send(Signal::Answer { sdp: answer_sdp }).await;
                            }
                            Err(e) => {
                                warn!("WebRTC answer failed, waiting for relay: {e:#}");
                            }
                        }
                    }
                    Signal::Ice { candidate: Some(c), sdp_mid, sdp_mline_index } => {
                        if let Some(p) = pc.as_ref() {
                            let init = RTCIceCandidateInit { candidate: c, sdp_mid, sdp_mline_index, ..Default::default() };
                            if let Err(e) = p.add_ice_candidate(init).await { debug!("add candidate: {e}"); }
                        }
                    }
                    Signal::Relay => {
                        info!("switching to the WebSocket relay");
                        mode.store(MODE_RELAY, Ordering::Relaxed);
                        *video.lock().await = None;
                        if let Some(old) = pc.take() { let _ = old.close().await; }
                        key.store(true, Ordering::Relaxed);
                    }
                    Signal::Keyframe => key.store(true, Ordering::Relaxed),
                    Signal::Input { event } => { let _ = input_tx.send(event); }
                    Signal::End { reason } => break format!("ended: {reason}"),
                    _ => {}
                }
            }
            Some(ev) = pc_rx.recv() => match ev {
                PcEvent::Ice(init) => send(Signal::Ice { candidate: Some(init.candidate), sdp_mid: init.sdp_mid, sdp_mline_index: init.sdp_mline_index }).await,
                PcEvent::State(s) => {
                    info!("peer connection {s}");
                    if s == RTCPeerConnectionState::Connected { key.store(true, Ordering::Relaxed); }
                }
                PcEvent::Input(ev) => { let _ = input_tx.send(ev); }
            },
            Some(s) = screen_rx.recv() => send(s).await,
        }
    };

    info!("session {} finished: {reason}", cfg.request.session_id);
    stop.store(true, Ordering::Relaxed);
    let _ = input_tx.send(InputEvent::ReleaseAll);
    if let Some(p) = pc.take() {
        let _ = p.close().await;
    }
    if !reason.starts_with("ended") && !reason.starts_with("server") {
        send(Signal::End { reason: "agent_error".into() }).await;
    }
    drop(out_tx);
    drop(frames_tx);
    pump.abort();
    let _ = tokio::time::timeout(Duration::from_secs(2), writer).await;
    if let Some(m) = media {
        let _ = tokio::task::spawn_blocking(move || m.join()).await;
    }
    Ok(reason)
}

fn h264_codec() -> RTCRtpCodec {
    RTCRtpCodec {
        mime_type: "video/H264".to_owned(),
        clock_rate: 90000,
        channels: 0,
        sdp_fmtp_line: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f".to_owned(),
        rtcp_feedback: vec![
            RTCPFeedback { typ: "nack".into(), parameter: "".into() },
            RTCPFeedback { typ: "nack".into(), parameter: "pli".into() },
            RTCPFeedback { typ: "ccm".into(), parameter: "fir".into() },
        ],
    }
}

/// Local IPv4 addresses to gather host candidates on.
fn udp_addrs() -> Vec<String> {
    let mut out: Vec<String> = local_ip_address::list_afinet_netifas()
        .map(|l| {
            l.into_iter()
                .filter(|(_, ip)| ip.is_ipv4() && !ip.is_loopback() && !ip.is_unspecified())
                .map(|(_, ip)| format!("{ip}:0"))
                .collect()
        })
        .unwrap_or_default();
    out.sort();
    out.dedup();
    if out.is_empty() {
        out.push("0.0.0.0:0".into());
    }
    out
}

async fn answer(
    cfg: &SessionConfig,
    offer_sdp: &str,
    tx: mpsc::UnboundedSender<PcEvent>,
) -> Result<(Box<dyn PeerConnection>, VideoOut, String)> {
    let codec = h264_codec();
    let mut media = MediaEngine::default();
    media.register_codec(
        RTCRtpCodecParameters { rtp_codec: codec.clone(), payload_type: 102, ..Default::default() },
        RtpCodecKind::Video,
    )?;
    let registry = register_default_interceptors(Registry::new(), &mut media)?;
    let settings = SettingEngineBuilder::new()
        .with_multicast_dns_mode(MulticastDnsMode::QueryOnly)
        .with_multicast_dns_timeout(Some(Duration::from_secs(5)))
        .build();
    let ice: Vec<RTCIceServer> = cfg
        .request
        .ice_servers
        .iter()
        .map(|s| RTCIceServer { urls: s.urls.clone(), username: s.username.clone(), credential: s.credential.clone() })
        .collect();
    let pc = PeerConnectionBuilder::new()
        .with_configuration(RTCConfigurationBuilder::new().with_ice_servers(ice).build())
        .with_media_engine(media)
        .with_interceptor_registry(registry)
        .with_setting_engine(settings)
        .with_handler(Arc::new(Handler { tx }))
        .with_udp_addrs(udp_addrs())
        .build()
        .await?;

    let ssrc = rand::random::<u32>();
    let track = Arc::new(TrackLocalStaticSample::new(
        Instant::now(),
        MediaStreamTrack::new(
            "rhis-screen".into(),
            "rhis-screen-video".into(),
            "screen".into(),
            RtpCodecKind::Video,
            vec![RTCRtpEncodingParameters {
                rtp_coding_parameters: RTCRtpCodingParameters { ssrc: Some(ssrc), ..Default::default() },
                codec: codec.clone(),
                ..Default::default()
            }],
        ),
    )?);
    let sender = pc.add_track(Arc::clone(&track) as Arc<dyn TrackLocal>).await?;
    pc.set_remote_description(RTCSessionDescription::offer(offer_sdp.to_string())?).await?;
    let ans = pc.create_answer(None).await?;
    pc.set_local_description(ans).await?;
    let local = pc.local_description().await.ok_or_else(|| anyhow!("no local description"))?;
    let pt = negotiated_pt(&sender).await?;
    Ok((Box::new(pc), VideoOut { track, ssrc, pt }, local.sdp))
}

async fn negotiated_pt(sender: &Arc<dyn RtpSender>) -> Result<PayloadType> {
    sender
        .get_parameters()
        .await?
        .rtp_parameters
        .codecs
        .first()
        .map(|c| c.payload_type)
        .ok_or_else(|| anyhow!("the viewer does not support H.264"))
}

/// Starts the capture thread: follows the input desktop, applies input between frames,
/// captures, skips unchanged frames and encodes.
#[allow(clippy::too_many_arguments)]
fn start_media(
    platform: Arc<dyn Platform>,
    fps: f32,
    input_rx: stdmpsc::Receiver<InputEvent>,
    key: Arc<AtomicBool>,
    stop: Arc<AtomicBool>,
    frames_tx: mpsc::Sender<EncodedFrame>,
    screen_tx: mpsc::UnboundedSender<Signal>,
) -> std::thread::JoinHandle<()> {
    std::thread::Builder::new()
        .name("rhis-capture".into())
        .spawn(move || {
            if let Err(e) = media_loop(&*platform, fps, input_rx, &key, &stop, &frames_tx, &screen_tx) {
                warn!("capture stopped: {e:#}");
                let _ = screen_tx.send(Signal::End { reason: "capture_failed".into() });
            }
        })
        .expect("spawn capture thread")
}

fn media_loop(
    platform: &dyn Platform,
    fps: f32,
    input_rx: stdmpsc::Receiver<InputEvent>,
    key: &AtomicBool,
    stop: &AtomicBool,
    frames_tx: &mpsc::Sender<EncodedFrame>,
    screen_tx: &mpsc::UnboundedSender<Signal>,
) -> Result<()> {
    platform.follow_input_desktop();
    let mut cap = platform.capturer()?;
    let mut inp = platform.input()?;
    let interval = Duration::from_secs_f32(1.0 / fps.clamp(1.0, 60.0));
    let mut monitors = cap.monitors();
    let mut monitors_at = Instant::now();
    let mut frame = Frame::default();
    let mut prev: Vec<u8> = Vec::new();
    let mut enc: Option<H264Encoder> = None;
    let mut next = Instant::now();
    let mut last_sent = Instant::now() - Duration::from_secs(60);
    let mut failures = 0u32;
    let mut announce = true;

    while !stop.load(Ordering::Relaxed) {
        if platform.follow_input_desktop() {
            info!("input desktop changed");
            cap.reset()?;
            key.store(true, Ordering::Relaxed);
        }
        // Apply input until it is time for the next frame.
        loop {
            let now = Instant::now();
            if now >= next {
                break;
            }
            match input_rx.recv_timeout(next - now) {
                Ok(InputEvent::Monitor { index }) => {
                    cap.select(index)?;
                    announce = true;
                }
                Ok(ev) => {
                    let m = monitors.get(cap.current()).cloned().unwrap_or_default();
                    if let Err(e) = inp.handle(&ev, &m) {
                        debug!("input: {e}");
                    }
                }
                Err(stdmpsc::RecvTimeoutError::Timeout) => break,
                Err(stdmpsc::RecvTimeoutError::Disconnected) => return Ok(()),
            }
        }
        let now = Instant::now();
        next += interval;
        if next < now {
            next = now + interval;
        }
        if monitors_at.elapsed() > Duration::from_secs(5) {
            let m = cap.monitors();
            if m != monitors {
                monitors = m;
                announce = true;
            }
            monitors_at = Instant::now();
        }
        if let Err(e) = cap.capture(&mut frame) {
            failures += 1;
            if failures > 50 {
                return Err(e);
            }
            debug!("capture: {e}");
            std::thread::sleep(Duration::from_millis(100));
            let _ = cap.reset();
            continue;
        }
        failures = 0;
        if enc.as_ref().map(|e| e.size()) != Some((frame.width, frame.height)) {
            enc = Some(H264Encoder::new(frame.width, frame.height, fps, default_bitrate(frame.width, frame.height))?);
            key.store(true, Ordering::Relaxed);
            prev.clear();
            announce = true;
        }
        if announce {
            monitors = cap.monitors();
            let _ = screen_tx.send(Signal::Screen {
                width: frame.width as u32,
                height: frame.height as u32,
                monitor: cap.current(),
                monitors: monitors.clone(),
            });
            announce = false;
        }
        let force = key.load(Ordering::Relaxed);
        let changed = frame.data != prev;
        // Unchanged screens are not encoded; one frame per second keeps the stream alive.
        if !changed && !force && last_sent.elapsed() < Duration::from_secs(1) {
            continue;
        }
        let at = Instant::now();
        if let Some(e) = enc.as_mut().unwrap().encode(&frame, force)? {
            if e.key {
                key.store(false, Ordering::Relaxed);
            }
            match frames_tx.try_send(EncodedFrame { data: e.data, key: e.key, at }) {
                Ok(()) => last_sent = at,
                Err(mpsc::error::TrySendError::Full(_)) => key.store(true, Ordering::Relaxed),
                Err(mpsc::error::TrySendError::Closed(_)) => return Ok(()),
            }
        }
        std::mem::swap(&mut prev, &mut frame.data);
    }
    Ok(())
}
