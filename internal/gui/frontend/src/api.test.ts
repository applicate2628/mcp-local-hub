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

import { postManifestCreate, postManifestValidate, getManifest, postManifestEdit, ManifestHashMismatchError } from "./api";

describe("postManifestCreate", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("resolves on 204", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 204,
      statusText: "No Content",
    }) as unknown as Response);
    await expect(postManifestCreate("demo", "name: demo")).resolves.toBeUndefined();
  });

  it("throws with backend error field on non-2xx", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: async () => ({ error: "manifest already exists" }),
    }) as unknown as Response);
    await expect(postManifestCreate("demo", "name: demo")).rejects.toThrow(/manifest already exists/);
  });

  it("serializes name + yaml into JSON body", async () => {
    const seen: { body?: string } = {};
    globalThis.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      seen.body = init?.body as string;
      return { ok: true, status: 204, statusText: "No Content" } as unknown as Response;
    });
    await postManifestCreate("demo", "name: demo\nkind: global\n");
    expect(JSON.parse(seen.body!)).toEqual({ name: "demo", yaml: "name: demo\nkind: global\n" });
  });
});

describe("postManifestValidate", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("returns warnings array on 200", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ warnings: ["no daemons declared"] }),
    }) as unknown as Response);
    const out = await postManifestValidate("name: x");
    expect(out).toEqual(["no daemons declared"]);
  });

  it("returns empty array when backend emits warnings:[]", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ warnings: [] }),
    }) as unknown as Response);
    const out = await postManifestValidate("name: demo\nkind: global\n");
    expect(out).toEqual([]);
  });

  it("throws on non-2xx with backend error text", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 400,
      statusText: "Bad Request",
      json: async () => ({ error: "invalid JSON" }),
    }) as unknown as Response);
    await expect(postManifestValidate("not-yaml-at-all")).rejects.toThrow(/invalid JSON/);
  });
});

describe("getManifest", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });
  it("returns {yaml, hash} on 200", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ yaml: "name: demo\n", hash: "abc" }),
    }) as unknown as Response);
    const out = await getManifest("demo");
    expect(out).toEqual({ yaml: "name: demo\n", hash: "abc" });
  });
  it("throws on non-2xx", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: async () => ({ error: "read failed" }),
    }) as unknown as Response);
    await expect(getManifest("demo")).rejects.toThrow(/read failed/);
  });
  it("URL-encodes the name", async () => {
    const seen: { url?: string } = {};
    globalThis.fetch = vi.fn(async (url: RequestInfo | URL) => {
      seen.url = url.toString();
      return { ok: true, status: 200, json: async () => ({ yaml: "", hash: "" }) } as unknown as Response;
    });
    await getManifest("weird name");
    expect(seen.url).toContain("name=weird%20name");
  });
});

describe("postManifestEdit", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });
  it("returns new hash on 200", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ hash: "new-hash-abc" }),
    }) as unknown as Response);
    const out = await postManifestEdit("demo", "name: demo\n", "old-hash");
    expect(out).toEqual({ hash: "new-hash-abc" });
  });
  it("throws ManifestHashMismatchError on 409 MANIFEST_HASH_MISMATCH", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 409,
      statusText: "Conflict",
      json: async () => ({ error: "hash mismatch", code: "MANIFEST_HASH_MISMATCH" }),
    }) as unknown as Response);
    await expect(postManifestEdit("demo", "name: demo\n", "stale")).rejects.toBeInstanceOf(ManifestHashMismatchError);
  });
  it("throws generic Error on other non-2xx", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: async () => ({ error: "disk full" }),
    }) as unknown as Response);
    await expect(postManifestEdit("demo", "name: demo\n", "hash")).rejects.toThrow(/disk full/);
  });
  it("sends name + yaml + expected_hash in JSON body", async () => {
    const seen: { body?: string } = {};
    globalThis.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      seen.body = init?.body as string;
      return { ok: true, status: 200, json: async () => ({ hash: "h" }) } as unknown as Response;
    });
    await postManifestEdit("demo", "name: demo", "hash123");
    expect(JSON.parse(seen.body!)).toEqual({
      name: "demo",
      yaml: "name: demo",
      expected_hash: "hash123",
    });
  });
  it("rejects a 200 response with missing hash (R3 safety)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({}),
    }) as unknown as Response);
    await expect(postManifestEdit("demo", "name: demo", "old"))
      .rejects.toThrow(/success response missing hash field/);
  });
});
