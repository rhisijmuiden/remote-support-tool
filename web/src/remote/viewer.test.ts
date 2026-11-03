import { describe, expect, it } from "vitest";
import { normalise } from "./viewer";

const rect = (left: number, top: number, width: number, height: number) => ({ left, top, width, height }) as DOMRect;

describe("normalise", () => {
  it("maps a letterboxed picture", () => {
    // 1600x900 element showing a 1280x1024 screen: pillarboxed, 1125 px wide.
    const r = rect(0, 0, 1600, 900);
    const mid = normalise(800, 450, r, 1280, 1024)!;
    expect(mid.x).toBeCloseTo(0.5);
    expect(mid.y).toBeCloseTo(0.5);
    expect(normalise(100, 450, r, 1280, 1024)).toBeNull();
    const left = normalise(237.5, 0, r, 1280, 1024)!;
    expect(left.x).toBeCloseTo(0);
  });
  it("needs a size", () => {
    expect(normalise(1, 1, rect(0, 0, 10, 10), 0, 0)).toBeNull();
  });
});
