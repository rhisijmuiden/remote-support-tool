// Remote session client for the browser: signaling over the session WebSocket, WebRTC video
// and an input data channel, and a WebSocket relay (H.264 decoded with WebCodecs) when WebRTC
// cannot connect. Protocol: docs/agent-protocol.md.

export type Transport = "p2p" | "turn" | "websocket";
export type ViewerState = "connecting" | "waiting" | "negotiating" | "connected" | "ended";
export type Monitor = { index: number; name: string; x: number; y: number; width: number; height: number; primary: boolean };
export type Screen = { width: number; height: number; monitor: number; monitors: Monitor[] };
export type InputEvent =
  | { t: "move"; x: number; y: number }
  | { t: "button"; button: number; down: boolean }
  | { t: "wheel"; dx: number; dy: number }
  | { t: "key"; code: string; down: boolean }
  | { t: "release_all" }
  | { t: "cad" }
  | { t: "monitor"; index: number };

export type ViewerEvents = {
  state: (s: ViewerState, reason?: string) => void;
  transport: (t: Transport) => void;
  screen: (s: Screen) => void;
  stream: (s: MediaStream) => void;
  /** Relay mode: draw this frame. */
  frame: (f: VideoFrame) => void;
};

type IceServer = { urls: string[]; username?: string; credential?: string };

/** How long WebRTC may take before the session falls back to the WebSocket relay. */
export const WEBRTC_TIMEOUT_MS = 12000;

export class RemoteViewer {
  private ws: WebSocket | null = null;
  private pc: RTCPeerConnection | null = null;
  private dc: RTCDataChannel | null = null;
  private decoder: VideoDecoder | null = null;
  private fallbackTimer = 0;
  private needKey = true;
  private _state: ViewerState = "connecting";
  transport: Transport | null = null;

  constructor(
    private sessionId: string,
    private iceServers: IceServer[],
    private on: Partial<ViewerEvents>,
    private opts: { forceRelay?: boolean } = {},
  ) {}

  get state() {
    return this._state;
  }

  private setState(s: ViewerState, reason?: string) {
    if (this._state === "ended") return;
    this._state = s;
    this.on.state?.(s, reason);
  }

  start() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}/api/live/${encodeURIComponent(this.sessionId)}/ws`);
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    ws.onopen = () => this.setState("waiting");
    ws.onclose = () => this.finish("connection_closed");
    ws.onmessage = (e) => {
      if (typeof e.data === "string") this.onSignal(JSON.parse(e.data));
      else this.onRelayFrame(new Uint8Array(e.data as ArrayBuffer));
    };
  }

  private send(m: object) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
  }

  private async onSignal(m: any) {
    switch (m.type) {
      case "ready":
        this.setState("negotiating");
        if (this.opts.forceRelay || !("RTCPeerConnection" in window)) this.useRelay();
        else this.startWebRTC().catch(() => this.useRelay());
        break;
      case "answer":
        await this.pc?.setRemoteDescription({ type: "answer", sdp: m.sdp });
        break;
      case "ice":
        if (this.pc && m.candidate) {
          await this.pc
            .addIceCandidate({ candidate: m.candidate, sdpMid: m.sdp_mid ?? undefined, sdpMLineIndex: m.sdp_mline_index ?? undefined })
            .catch(() => {});
        }
        break;
      case "screen":
        this.on.screen?.(m as Screen);
        break;
      case "end":
        this.finish(m.reason || "ended");
        break;
    }
  }

  private async startWebRTC() {
    const pc = new RTCPeerConnection({ iceServers: this.iceServers, bundlePolicy: "max-bundle" });
    this.pc = pc;
    const tr = pc.addTransceiver("video", { direction: "recvonly" });
    // Prefer H.264, the only codec the agent sends.
    const caps = RTCRtpReceiver.getCapabilities?.("video");
    if (caps && tr.setCodecPreferences) {
      const h264 = caps.codecs.filter((c) => /h264/i.test(c.mimeType) && /packetization-mode=1/.test(c.sdpFmtpLine ?? ""));
      if (h264.length) tr.setCodecPreferences(h264);
    }
    this.dc = pc.createDataChannel("input", { ordered: true });
    pc.onicecandidate = (e) => {
      if (e.candidate) this.send({ type: "ice", candidate: e.candidate.candidate, sdp_mid: e.candidate.sdpMid, sdp_mline_index: e.candidate.sdpMLineIndex });
    };
    pc.ontrack = (e) => this.on.stream?.(e.streams[0] ?? new MediaStream([e.track]));
    pc.onconnectionstatechange = () => {
      if (pc !== this.pc) return;
      if (pc.connectionState === "connected") {
        clearTimeout(this.fallbackTimer);
        this.setState("connected");
        this.detectTransport(pc);
      } else if (pc.connectionState === "failed") {
        this.useRelay();
      }
    };
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    this.send({ type: "offer", sdp: offer.sdp });
    this.fallbackTimer = window.setTimeout(() => {
      if (this.pc && this.pc.connectionState !== "connected") this.useRelay();
    }, WEBRTC_TIMEOUT_MS);
  }

  private async detectTransport(pc: RTCPeerConnection) {
    let t: Transport = "p2p";
    try {
      const stats = await pc.getStats();
      let pairId = "";
      stats.forEach((s: any) => {
        if (s.type === "transport" && s.selectedCandidatePairId) pairId = s.selectedCandidatePairId;
      });
      stats.forEach((s: any) => {
        if (!pairId && s.type === "candidate-pair" && s.nominated && s.state === "succeeded") pairId = s.id;
      });
      const pair: any = pairId ? stats.get(pairId) : null;
      const local: any = pair ? stats.get(pair.localCandidateId) : null;
      const remote: any = pair ? stats.get(pair.remoteCandidateId) : null;
      if (local?.candidateType === "relay" || remote?.candidateType === "relay") t = "turn";
    } catch {
      /* keep p2p */
    }
    this.setTransport(t);
  }

  private setTransport(t: Transport) {
    this.transport = t;
    this.send({ type: "transport", transport: t });
    this.on.transport?.(t);
  }

  /** Switches to the WebSocket relay over HTTPS (works when UDP is blocked). */
  useRelay() {
    if (this.transport === "websocket" || this._state === "ended") return;
    clearTimeout(this.fallbackTimer);
    this.pc?.close();
    this.pc = null;
    this.dc = null;
    if (!("VideoDecoder" in window)) {
      this.finish("relay_unsupported");
      return;
    }
    this.decoder = new VideoDecoder({
      output: (f) => {
        if (this.on.frame) this.on.frame(f);
        else f.close();
      },
      error: () => {
        this.needKey = true;
        this.send({ type: "keyframe" });
      },
    });
    // Annex B input: no description, the stream carries SPS/PPS in front of each key frame.
    this.decoder.configure({ codec: "avc1.42E01F", optimizeForLatency: true, hardwareAcceleration: "no-preference" });
    this.needKey = true;
    this.send({ type: "relay" });
    this.setTransport("websocket");
    this.setState("connected");
  }

  private onRelayFrame(b: Uint8Array) {
    const d = this.decoder;
    if (!d || b.length < 10) return;
    if (d.state === "closed") return;
    const key = (b[0] & 1) === 1;
    if (this.needKey && !key) return;
    if (d.decodeQueueSize > 4) {
      // Falling behind: drop until the next key frame.
      this.needKey = true;
      this.send({ type: "keyframe" });
      return;
    }
    this.needKey = false;
    const ts = Number(new DataView(b.buffer, b.byteOffset + 1, 8).getBigUint64(0));
    d.decode(new EncodedVideoChunk({ type: key ? "key" : "delta", timestamp: ts, data: b.subarray(9) }));
  }

  input(ev: InputEvent) {
    if (this._state !== "connected") return;
    if (this.dc?.readyState === "open") this.dc.send(JSON.stringify(ev));
    else this.send({ type: "input", event: ev });
  }

  stop(reason = "viewer_closed") {
    this.send({ type: "end", reason });
    this.finish(reason);
  }

  private finish(reason: string) {
    if (this._state === "ended") return;
    clearTimeout(this.fallbackTimer);
    this.setState("ended", reason);
    this._state = "ended";
    this.pc?.close();
    try {
      this.decoder?.close();
    } catch {
      /* already closed */
    }
    this.ws?.close();
  }
}

/** Maps a pointer position in an element showing a `w`x`h` picture with object-fit: contain to 0..1. */
export function normalise(clientX: number, clientY: number, rect: DOMRect, w: number, h: number) {
  if (!w || !h) return null;
  const scale = Math.min(rect.width / w, rect.height / h);
  const dw = w * scale;
  const dh = h * scale;
  const ox = rect.left + (rect.width - dw) / 2;
  const oy = rect.top + (rect.height - dh) / 2;
  const x = (clientX - ox) / dw;
  const y = (clientY - oy) / dh;
  if (x < 0 || y < 0 || x > 1 || y > 1) return null;
  return { x, y };
}
