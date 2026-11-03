// ChunkUploader sends recording chunks to the server strictly in order, one at a time,
// retrying with backoff. The server accepts a repeated chunk, so a retry after a lost
// response is safe.
export type UploadState = "idle" | "recording" | "finishing" | "done" | "failed";

type Fetch = typeof fetch;

export class ChunkUploader {
  private queue: Blob[] = [];
  private seq = 0;
  private busy = false;
  private finishing = false;
  private waiters: Array<() => void> = [];
  state: UploadState = "recording";
  error: string | null = null;

  constructor(
    private sessionId: number,
    private mime: string,
    private onChange: (s: UploadState, err: string | null) => void = () => {},
    private doFetch: Fetch = (...a) => fetch(...a),
    private delays: number[] = [1000, 2000, 5000, 10000, 20000, 30000],
  ) {}

  push(b: Blob) {
    if (b.size === 0 || this.state === "failed" || this.state === "done") return;
    this.queue.push(b);
    void this.pump();
  }

  /** finish waits for every queued chunk and tells the server the recording is complete. */
  async finish(): Promise<void> {
    if (this.state === "done" || this.state === "failed") return;
    this.finishing = true;
    this.set("finishing");
    await this.drain();
    // pump() may have failed while we waited.
    if ((this.state as UploadState) === "failed") return;
    const ok = await this.send("POST", `/api/sessions/${this.sessionId}/recording/finish`, undefined, [409]);
    this.set(ok ? "done" : "failed");
  }

  get pending() {
    return this.queue.length + (this.busy ? 1 : 0);
  }

  private set(s: UploadState, err: string | null = this.error) {
    this.state = s;
    this.error = err;
    this.onChange(s, err);
  }

  private drain(): Promise<void> {
    if (!this.busy && this.queue.length === 0) return Promise.resolve();
    return new Promise((r) => this.waiters.push(r));
  }

  private async pump() {
    if (this.busy) return;
    this.busy = true;
    while (this.queue.length > 0 && this.state !== "failed") {
      const b = this.queue[0];
      const ok = await this.send("PUT", `/api/sessions/${this.sessionId}/recording/${this.seq}`, b);
      if (!ok) break;
      this.queue.shift();
      this.seq++;
    }
    this.busy = false;
    if (this.queue.length === 0 || this.state === "failed") {
      const w = this.waiters;
      this.waiters = [];
      w.forEach((f) => f());
    }
    if (!this.finishing && this.state === "recording") this.onChange(this.state, null);
  }

  // send returns true on success; okCodes are statuses that also count as done.
  private async send(method: string, url: string, body?: Blob, okCodes: number[] = []): Promise<boolean> {
    for (let attempt = 0; ; attempt++) {
      try {
        const res = await this.doFetch(url, {
          method,
          credentials: "same-origin",
          headers: { "X-Requested-With": "rhis-remote", ...(body ? { "Content-Type": this.mime } : {}) },
          body,
        });
        if (res.ok || okCodes.includes(res.status)) return true;
        // 4xx other than 408/429 will not get better by retrying.
        if (res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429) {
          const data = await res.json().catch(() => ({}));
          this.set("failed", data.error ?? `http_${res.status}`);
          return false;
        }
      } catch {
        /* network error: retry */
      }
      if (attempt >= this.delays.length) {
        this.set("failed", "upload_failed");
        return false;
      }
      await new Promise((r) => setTimeout(r, this.delays[attempt]));
    }
  }
}

/** pickMime returns the best recording format this browser supports. */
export function pickMime(isSupported: (m: string) => boolean = (m) => typeof MediaRecorder !== "undefined" && MediaRecorder.isTypeSupported(m)): string | null {
  for (const m of ['video/webm;codecs="vp9"', 'video/webm;codecs="vp8"', "video/webm", 'video/mp4;codecs="avc1"', "video/mp4"]) {
    if (isSupported(m)) return m;
  }
  return null;
}
