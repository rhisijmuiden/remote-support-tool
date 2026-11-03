import { describe, expect, it } from "vitest";
import { ChunkUploader, pickMime } from "./uploader";

function fakeFetch(plan: Array<number | "net">) {
  const calls: string[] = [];
  const f = (async (url: string, init: RequestInit) => {
    calls.push(`${init.method} ${url}`);
    const step = plan.shift() ?? 204;
    if (step === "net") throw new TypeError("network");
    return new Response(step === 204 ? null : "{}", { status: step });
  }) as unknown as typeof fetch;
  return { f, calls };
}

describe("ChunkUploader", () => {
  it("uploads in order, retries network errors and finishes", async () => {
    const { f, calls } = fakeFetch([204, "net", 503, 204, 204, 204]);
    const u = new ChunkUploader(7, "video/webm", undefined, f, [0, 0, 0]);
    u.push(new Blob(["a"]));
    u.push(new Blob(["b"]));
    u.push(new Blob(["c"]));
    await u.finish();
    expect(u.state).toBe("done");
    expect(calls).toEqual([
      "PUT /api/sessions/7/recording/0",
      "PUT /api/sessions/7/recording/1",
      "PUT /api/sessions/7/recording/1",
      "PUT /api/sessions/7/recording/1",
      "PUT /api/sessions/7/recording/2",
      "POST /api/sessions/7/recording/finish",
    ]);
  });

  it("stops on a client error", async () => {
    const { f } = fakeFetch([409]);
    const u = new ChunkUploader(1, "video/webm", undefined, f, [0]);
    u.push(new Blob(["a"]));
    await u.finish();
    expect(u.state).toBe("failed");
  });

  it("gives up after the retries", async () => {
    const { f } = fakeFetch(["net", "net", "net"]);
    const u = new ChunkUploader(1, "video/webm", undefined, f, [0, 0]);
    u.push(new Blob(["a"]));
    await u.finish();
    expect(u.state).toBe("failed");
    expect(u.error).toBe("upload_failed");
  });

  it("ignores empty blobs", async () => {
    const { f, calls } = fakeFetch([]);
    const u = new ChunkUploader(1, "video/webm", undefined, f, [0]);
    u.push(new Blob([]));
    await u.finish();
    expect(calls).toEqual(["POST /api/sessions/1/recording/finish"]);
  });
});

describe("pickMime", () => {
  it("prefers VP9 WebM and falls back to MP4", () => {
    expect(pickMime(() => true)).toBe('video/webm;codecs="vp9"');
    expect(pickMime((m) => m.startsWith("video/mp4"))).toBe('video/mp4;codecs="avc1"');
    expect(pickMime(() => false)).toBeNull();
  });
});
