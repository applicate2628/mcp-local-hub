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

import { installMarketplaceEntry } from "./api";

describe("installMarketplaceEntry", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("maps a 201 hub success to kind:'hub-installed' with name + port", async () => {
    const seen: { url?: string; body?: string } = {};
    globalThis.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      seen.url = url.toString();
      seen.body = init?.body as string;
      return {
        ok: true,
        status: 201,
        statusText: "Created",
        json: async () => ({ name: "git", port: 9201, mode: "hub" }),
      } as unknown as Response;
    });
    const out = await installMarketplaceEntry({ id: "git", mode: "hub" });
    expect(out).toEqual({ kind: "hub-installed", name: "git", port: 9201 });
    expect(seen.url).toBe("/api/marketplace/install");
    expect(JSON.parse(seen.body!)).toMatchObject({ id: "git", mode: "hub" });
  });

  it("maps a 409 NAME_CONFLICT to kind:'name-conflict' (not a throw)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 409,
      statusText: "Conflict",
      json: async () => ({ error_code: "NAME_CONFLICT", suggested_name: "git-2" }),
    }) as unknown as Response);
    const out = await installMarketplaceEntry({ id: "git", mode: "hub" });
    expect(out).toEqual({ kind: "name-conflict", suggestedName: "git-2" });
  });

  it("carries the suggested name through on a hub retry", async () => {
    const seen: { body?: string } = {};
    globalThis.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      seen.body = init?.body as string;
      return {
        ok: true,
        status: 201,
        statusText: "Created",
        json: async () => ({ name: "git-2", port: 9202, mode: "hub" }),
      } as unknown as Response;
    });
    const out = await installMarketplaceEntry({ id: "git", mode: "hub", name: "git-2" });
    expect(out).toEqual({ kind: "hub-installed", name: "git-2", port: 9202 });
    expect(JSON.parse(seen.body!)).toMatchObject({ id: "git", mode: "hub", name: "git-2" });
  });

  it("maps a 200 direct all-ok to kind:'direct' with partial=false", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({
        clients_updated: ["claude-code", "cursor"],
        clients_failed: [],
        mode: "direct",
      }),
    }) as unknown as Response);
    const out = await installMarketplaceEntry({
      id: "remote",
      mode: "direct",
      clients: ["claude-code", "cursor"],
    });
    expect(out).toEqual({
      kind: "direct",
      partial: false,
      clientsUpdated: ["claude-code", "cursor"],
      clientsFailed: [],
    });
  });

  it("maps a 207 direct partial to kind:'direct' with partial=true + failed list", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 207,
      statusText: "Multi-Status",
      json: async () => ({
        clients_updated: ["claude-code"],
        clients_failed: [{ client: "vscode", error: "symlink" }],
        mode: "direct",
      }),
    }) as unknown as Response);
    const out = await installMarketplaceEntry({
      id: "remote",
      mode: "direct",
      clients: ["claude-code", "vscode"],
    });
    expect(out).toMatchObject({
      kind: "direct",
      partial: true,
      clientsUpdated: ["claude-code"],
      clientsFailed: [{ client: "vscode", error: "symlink" }],
    });
  });

  it("throws the backend envelope on an unmodelled failure (502 CATALOG_UNAVAILABLE)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 502,
      statusText: "Bad Gateway",
      json: async () => ({ error: "marketplace catalog unavailable", code: "CATALOG_UNAVAILABLE" }),
    }) as unknown as Response);
    await expect(installMarketplaceEntry({ id: "git", mode: "hub" })).rejects.toThrow(
      /marketplace catalog unavailable/,
    );
  });
});

import { validatePath } from "./api";

describe("validatePath", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("URL-encodes the path query param", async () => {
    const seen: { url?: string } = {};
    globalThis.fetch = vi.fn(async (url: RequestInfo | URL) => {
      seen.url = url.toString();
      return {
        ok: true,
        status: 200,
        json: async () => ({ path: "C:/Program Files", exists: true, is_dir: true }),
      } as unknown as Response;
    });
    await validatePath("C:/Program Files");
    expect(seen.url).toContain("path=C%3A%2FProgram%20Files");
  });

  it("returns exists+is_dir for an existing directory", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ path: "/home/x", exists: true, is_dir: true }),
    }) as unknown as Response);
    const out = await validatePath("/home/x");
    expect(out).toEqual({ path: "/home/x", exists: true, is_dir: true, error: undefined });
  });

  it("returns exists:false for a missing path WITHOUT throwing", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ path: "/nope", exists: false, is_dir: false }),
    }) as unknown as Response);
    const out = await validatePath("/nope");
    expect(out.exists).toBe(false);
    expect(out.is_dir).toBe(false);
  });

  it("normalizes a partial/older body to a complete result", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({}),
    }) as unknown as Response);
    const out = await validatePath("/x");
    // Missing fields default to safe falses; path echoes the request arg.
    expect(out).toEqual({ path: "/x", exists: false, is_dir: false, error: undefined });
  });

  it("surfaces a stat error string when present", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ path: "/root/secret", exists: false, is_dir: false, error: "permission denied" }),
    }) as unknown as Response);
    const out = await validatePath("/root/secret");
    expect(out.error).toBe("permission denied");
  });

  it("throws the backend envelope on 400 PATH_INVALID", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 400,
      statusText: "Bad Request",
      json: async () => ({ error: "path contains control characters", code: "PATH_INVALID" }),
    }) as unknown as Response);
    await expect(validatePath("/tmp/foo\nbar")).rejects.toThrow(/path contains control characters/);
  });
});
