// useSessionRecorder records the remote screen in the technician's browser and uploads
// it to the server while the session runs.
//
//   const rec = useSessionRecorder(session.id, session.recording === "recording", () => videoEl.srcObject as MediaStream);
//   // WebSocket fallback renders to a canvas: () => canvas.captureStream(15)
//   await rec.stop(); // before tearing the connection down
//
// The server decides whether a session is recorded (portal setting + the technician's
// choice); the agent shows the customer a "this session is being recorded" banner.
import { useCallback, useEffect, useRef, useState } from "react";
import { ChunkUploader, UploadState, pickMime } from "./uploader";

export type RecorderState = UploadState | "unsupported";

/** Chunk length in milliseconds. Shorter loses less on a crash, longer means fewer requests. */
export const TIMESLICE = 5000;

export function useSessionRecorder(sessionId: number | null, enabled: boolean, getStream: () => MediaStream | null) {
  const [state, setState] = useState<RecorderState>("idle");
  const [error, setError] = useState<string | null>(null);
  const rec = useRef<MediaRecorder | null>(null);
  const up = useRef<ChunkUploader | null>(null);
  const streamFn = useRef(getStream);
  streamFn.current = getStream;

  const stop = useCallback(async () => {
    const r = rec.current;
    const u = up.current;
    rec.current = null;
    if (r && r.state !== "inactive") {
      // The last dataavailable fires before stop.
      await new Promise<void>((done) => {
        r.addEventListener("stop", () => done(), { once: true });
        r.stop();
      });
    }
    await u?.finish();
  }, []);

  useEffect(() => {
    if (!enabled || sessionId == null) return;
    let cancelled = false;
    let timer: ReturnType<typeof setInterval> | undefined;
    const start = () => {
      const stream = streamFn.current();
      if (!stream || stream.getVideoTracks().length === 0) return false;
      const mime = pickMime();
      if (!mime) {
        setState("unsupported");
        return true;
      }
      const u = new ChunkUploader(sessionId, mime, (s, e) => {
        if (!cancelled) {
          setState(s);
          setError(e);
        }
      });
      const r = new MediaRecorder(stream, { mimeType: mime, videoBitsPerSecond: 1_500_000 });
      r.ondataavailable = (e) => u.push(e.data);
      r.start(TIMESLICE);
      rec.current = r;
      up.current = u;
      setState("recording");
      return true;
    };
    // The stream may arrive after the session row exists; poll briefly for it.
    if (!start()) {
      timer = setInterval(() => start() && clearInterval(timer), 500);
    }
    const unload = () => rec.current?.state === "recording" && rec.current.requestData();
    window.addEventListener("pagehide", unload);
    return () => {
      cancelled = true;
      clearInterval(timer);
      window.removeEventListener("pagehide", unload);
      void stop();
    };
  }, [sessionId, enabled, stop]);

  return { state, error, stop };
}
