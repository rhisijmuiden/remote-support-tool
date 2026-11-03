import { describe, expect, it } from "vitest";
import en from "./en.json";
import nl from "./nl.json";

function keys(o: Record<string, unknown>, p = ""): string[] {
  return Object.entries(o).flatMap(([k, v]) => (typeof v === "object" && v ? keys(v as Record<string, unknown>, p + k + ".") : [p + k]));
}

describe("translations", () => {
  it("Dutch has exactly the same keys as English", () => {
    expect(keys(nl).sort()).toEqual(keys(en).sort());
  });
  it("no empty strings", () => {
    for (const [, v] of Object.entries({ ...en, ...nl })) expect(v).not.toBe("");
  });
});
