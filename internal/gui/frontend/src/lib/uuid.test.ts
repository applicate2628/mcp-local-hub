import { describe, it, expect } from "vitest";
import { generateUUID } from "./uuid";

describe("generateUUID", () => {
  it("produces a non-empty string", () => {
    const id = generateUUID();
    expect(typeof id).toBe("string");
    expect(id.length).toBeGreaterThan(0);
  });
  it("produces different ids on successive calls", () => {
    const a = generateUUID();
    const b = generateUUID();
    expect(a).not.toBe(b);
  });
  it("matches a v4-ish shape (8-4-4-4-12 hex) when crypto.randomUUID is native", () => {
    const id = generateUUID();
    // crypto.randomUUID output is always this shape; the fallback may differ
    // but must still be unique. Assert SHAPE only when native is present.
    if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
      expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    }
  });
});
