import { describe, expect, it, vi, beforeEach } from "vitest";
import { fetchOrThrow } from "./api";

describe("fetchOrThrow", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("returns parsed JSON on 200 + object shape", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      statusText: "OK",
      json: async () => ({ foo: 1 }),
    }) as unknown as Response);
    const out = await fetchOrThrow<{ foo: number }>("/x", "object");
    expect(out).toEqual({ foo: 1 });
  });

  it("returns parsed JSON on 200 + array shape", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      statusText: "OK",
      json: async () => [1, 2, 3],
    }) as unknown as Response);
    const out = await fetchOrThrow<number[]>("/y", "array");
    expect(out).toEqual([1, 2, 3]);
  });

  it("throws with the error envelope's message on non-ok", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      statusText: "Bad Request",
      json: async () => ({ error: "invalid server name" }),
    }) as unknown as Response);
    await expect(fetchOrThrow("/z", "object")).rejects.toThrow(/invalid server name/);
  });

  it("throws on array shape mismatch", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      statusText: "OK",
      json: async () => ({ notAnArray: true }),
    }) as unknown as Response);
    await expect(fetchOrThrow("/q", "array")).rejects.toThrow(/expected array/);
  });

  it("throws on object shape mismatch (array received)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      statusText: "OK",
      json: async () => [1, 2],
    }) as unknown as Response);
    await expect(fetchOrThrow("/p", "object")).rejects.toThrow(/expected object/);
  });
});
