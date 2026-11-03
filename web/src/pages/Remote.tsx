import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Icon } from "../components/Icon";
import { errorText } from "../components/ui";
import { InputEvent, RemoteViewer, Screen, Transport, ViewerState, normalise } from "../remote/viewer";

type SessionInfo = {
  session_id: string;
  device_id: number;
  hostname: string;
  device_code: string;
  kind: string;
  recording: boolean;
  log_id: number;
  ice_servers: { urls: string[]; username?: string; credential?: string }[];
};

export function formatCode(c: string) {
  return c.length === 9 ? `${c.slice(0, 3)} ${c.slice(3, 6)} ${c.slice(6)}` : c;
}

const buttonOf = (b: number) => (b === 1 ? 1 : b === 2 ? 2 : 0);

export function Remote() {
  const { t } = useTranslation();
  const { sid = "" } = useParams();
  const nav = useNavigate();
  const [info, setInfo] = useState<SessionInfo | null>(null);
  const [err, setErr] = useState<unknown>(null);
  const [state, setState] = useState<ViewerState>("connecting");
  const [reason, setReason] = useState("");
  const [transport, setTransport] = useState<Transport | null>(null);
  const [screen, setScreen] = useState<Screen | null>(null);
  const [viewOnly, setViewOnly] = useState(false);
  const [relayMode, setRelayMode] = useState(false);
  const viewer = useRef<RemoteViewer | null>(null);
  const video = useRef<HTMLVideoElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const stage = useRef<HTMLDivElement>(null);
  const pending = useRef<{ x: number; y: number } | null>(null);
  const viewOnlyRef = useRef(false);
  viewOnlyRef.current = viewOnly;

  useEffect(() => {
    let v: RemoteViewer | null = null;
    api.get<SessionInfo>(`/sessions/${encodeURIComponent(sid)}`).then(
      (si) => {
        setInfo(si);
        document.title = `${si.hostname} · ${formatCode(si.device_code)}`;
        const forceRelay = new URLSearchParams(location.search).get("relay") === "1";
        v = new RemoteViewer(
          si.session_id,
          si.ice_servers,
          {
            state: (s, r) => {
              setState(s);
              if (r) setReason(r);
            },
            transport: (tr) => {
              setTransport(tr);
              setRelayMode(tr === "websocket");
            },
            screen: setScreen,
            stream: (s) => {
              if (video.current) {
                video.current.srcObject = s;
                video.current.play().catch(() => {});
              }
            },
            frame: (f) => {
              const c = canvas.current;
              if (c) {
                if (c.width !== f.displayWidth || c.height !== f.displayHeight) {
                  c.width = f.displayWidth;
                  c.height = f.displayHeight;
                }
                c.getContext("2d")?.drawImage(f, 0, 0);
              }
              f.close();
            },
          },
          { forceRelay },
        );
        viewer.current = v;
        v.start();
      },
      (e) => setErr(e),
    );
    const leave = () => v?.stop("viewer_closed");
    window.addEventListener("pagehide", leave);
    return () => {
      window.removeEventListener("pagehide", leave);
      v?.stop("viewer_closed");
    };
  }, [sid]);

  /** The stream to record: the WebRTC video, or the relay canvas. */
  const getStream = useCallback((): MediaStream | null => {
    if (relayMode) return canvas.current?.captureStream(15) ?? null;
    return (video.current?.srcObject as MediaStream | null) ?? null;
  }, [relayMode]);
  void getStream; // used by the session recorder

  const send = (ev: InputEvent) => {
    if (!viewOnlyRef.current) viewer.current?.input(ev);
  };

  const pos = (e: { clientX: number; clientY: number }) => {
    const el = relayMode ? canvas.current : video.current;
    if (!el || !screen) return null;
    return normalise(e.clientX, e.clientY, el.getBoundingClientRect(), screen.width, screen.height);
  };

  // Coalesce pointer moves to one per animation frame.
  useEffect(() => {
    let raf = 0;
    const tick = () => {
      if (pending.current) {
        send({ t: "move", ...pending.current });
        pending.current = null;
      }
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  });

  // Keyboard goes to the remote machine while the stage has focus.
  useEffect(() => {
    const el = stage.current;
    if (!el) return;
    const key = (down: boolean) => (e: KeyboardEvent) => {
      e.preventDefault();
      send({ t: "key", code: e.code, down });
    };
    const kd = key(true);
    const ku = key(false);
    const blur = () => send({ t: "release_all" });
    el.addEventListener("keydown", kd);
    el.addEventListener("keyup", ku);
    el.addEventListener("blur", blur);
    const wheel = (e: WheelEvent) => {
      e.preventDefault();
      const k = e.deltaMode === 1 ? 40 : e.deltaMode === 2 ? 800 : 1;
      send({ t: "wheel", dx: e.deltaX * k, dy: e.deltaY * k });
    };
    el.addEventListener("wheel", wheel, { passive: false });
    return () => {
      el.removeEventListener("keydown", kd);
      el.removeEventListener("keyup", ku);
      el.removeEventListener("blur", blur);
      el.removeEventListener("wheel", wheel);
    };
  });

  const fullscreen = async () => {
    const el = stage.current?.parentElement;
    if (!el) return;
    if (document.fullscreenElement) {
      await document.exitFullscreen();
      return;
    }
    await el.requestFullscreen().catch(() => {});
    // In fullscreen, Chromium lets the page capture keys like Alt+Tab and the Windows key.
    (navigator as any).keyboard?.lock?.().catch(() => {});
    stage.current?.focus();
  };

  const disconnect = () => {
    viewer.current?.stop("viewer_closed");
    nav("/devices");
  };

  const transportLabel = transport ? t("remote.transport_" + transport) : "";
  const ended = state === "ended";

  return (
    <div className="remote">
      <header className="remote-bar">
        <Link to="/devices" className="btn-icon" aria-label={t("remote.back")} title={t("remote.back")}>
          <Icon name="back" />
        </Link>
        <div className="remote-title">
          <strong>{info?.hostname ?? "…"}</strong>
          <span className="mono muted">{info ? formatCode(info.device_code) : ""}</span>
        </div>
        {transport && (
          <span className={"pill " + (transport === "p2p" ? "ok" : "neutral")} title={t("remote.encrypted")}>
            <Icon name="lock" /> {transportLabel}
          </span>
        )}
        {info?.recording && <span className="pill rec">● {t("remote.recording")}</span>}
        <span className="spacer" />
        {screen && screen.monitors.length > 1 && (
          <select
            className="input input-sm"
            aria-label={t("remote.monitor")}
            value={screen.monitor}
            onChange={(e) => send({ t: "monitor", index: Number(e.target.value) })}
          >
            {screen.monitors.map((m) => (
              <option key={m.index} value={m.index}>
                {m.primary ? t("remote.monitor_primary", { n: m.index + 1 }) : t("remote.monitor_n", { n: m.index + 1 })} · {m.width}×{m.height}
              </option>
            ))}
          </select>
        )}
        <button className="btn btn-secondary btn-sm" disabled={state !== "connected" || viewOnly} onClick={() => send({ t: "cad" })}>
          Ctrl+Alt+Del
        </button>
        <button
          className={"btn-icon" + (viewOnly ? " on" : "")}
          aria-pressed={viewOnly}
          onClick={() => setViewOnly(!viewOnly)}
          title={t("remote.view_only")}
          aria-label={t("remote.view_only")}
        >
          <Icon name="eye" />
        </button>
        <button className="btn-icon" onClick={fullscreen} title={t("remote.fullscreen")} aria-label={t("remote.fullscreen")}>
          <Icon name="fullscreen" />
        </button>
        <button className="btn btn-danger btn-sm" onClick={disconnect}>
          {t("remote.disconnect")}
        </button>
      </header>
      <div
        className="remote-stage"
        ref={stage}
        tabIndex={0}
        onPointerMove={(e) => {
          const p = pos(e);
          if (p) pending.current = p;
        }}
        onPointerDown={(e) => {
          stage.current?.focus();
          const p = pos(e);
          if (!p) return;
          (e.target as Element).setPointerCapture?.(e.pointerId);
          send({ t: "move", ...p });
          send({ t: "button", button: buttonOf(e.button), down: true });
        }}
        onPointerUp={(e) => send({ t: "button", button: buttonOf(e.button), down: false })}
        onContextMenu={(e) => e.preventDefault()}
      >
        <video ref={video} muted playsInline autoPlay hidden={relayMode} />
        <canvas ref={canvas} hidden={!relayMode} />
        {state !== "connected" && (
          <div className="remote-overlay" role="status">
            {err ? (
              <>
                <strong>{errorText(t, err)}</strong>
                <Link className="btn btn-primary" to="/devices">
                  {t("remote.back")}
                </Link>
              </>
            ) : ended ? (
              <>
                <strong>{t("remote.ended")}</strong>
                <span className="muted">{t("remote.reason." + reason.split(":")[0], { defaultValue: reason })}</span>
                <div className="row">
                  <Link className="btn btn-primary" to="/devices">
                    {t("remote.back")}
                  </Link>
                </div>
              </>
            ) : (
              <>
                <span className="spinner" aria-hidden="true" />
                <strong>{t("remote.state_" + state)}</strong>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
