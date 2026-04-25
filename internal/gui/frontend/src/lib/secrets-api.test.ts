import { describe, expect, it, beforeEach, vi } from "vitest";
import { addSecret, deleteSecret, getSecrets, restartSecret, rotateSecret, secretsInit } from "./secrets-api";

const mockFetch = vi.fn();
beforeEach(() => {
  mockFetch.mockReset();
  globalThis.fetch = mockFetch as unknown as typeof fetch;
});

describe("getSecrets", () => {
  it("parses the envelope on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        vault_state: "ok",
        secrets: [{ name: "K1", state: "present", used_by: [{ server: "s1", env_var: "OPENAI" }] }],
        manifest_errors: [],
      }),
    });

    const env = await getSecrets();
    expect(env.vault_state).toBe("ok");
    expect(env.secrets).toHaveLength(1);
    expect(env.secrets[0].used_by[0].server).toBe("s1");
  });

  it("throws on 5xx", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: "boom", code: "SECRETS_LIST_FAILED" }),
    });
    await expect(getSecrets()).rejects.toThrow(/SECRETS_LIST_FAILED|boom/);
  });
});

describe("secretsInit", () => {
  it("returns body on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ vault_state: "ok" }),
    });
    const res = await secretsInit();
    expect(res.vault_state).toBe("ok");
  });

  it("throws on 409 SECRETS_INIT_BLOCKED", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: "blocked", code: "SECRETS_INIT_BLOCKED" }),
    });
    await expect(secretsInit()).rejects.toMatchObject({ code: "SECRETS_INIT_BLOCKED", status: 409 });
  });
});

describe("addSecret", () => {
  it("returns void on 201", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 201, json: async () => ({}) });
    await expect(addSecret("K1", "v")).resolves.toBeUndefined();
  });

  it("throws on 409 SECRETS_KEY_EXISTS", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: "dup", code: "SECRETS_KEY_EXISTS" }),
    });
    await expect(addSecret("K1", "v")).rejects.toMatchObject({ code: "SECRETS_KEY_EXISTS" });
  });
});

describe("rotateSecret", () => {
  it("returns body on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ vault_updated: true, restart_results: [] }),
    });
    const res = await rotateSecret("K1", "v", false);
    expect(res.vault_updated).toBe(true);
  });

  it("returns body on 207", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 207,
      json: async () => ({
        vault_updated: true,
        restart_results: [{ task_name: "x", error: "fail" }],
      }),
    });
    const res = await rotateSecret("K1", "v", true);
    expect(res.restart_results[0].error).toBe("fail");
  });

  it("returns body on 500 RESTART_FAILED when vault was committed (preserves code+error)", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({
        vault_updated: true,
        code: "RESTART_FAILED",
        error: "scheduler unavailable",
        restart_results: [],
      }),
    });
    const res = await rotateSecret("K1", "v", true);
    expect(res.vault_updated).toBe(true);
    expect(res.restart_results).toHaveLength(0);
    // Codex PR #18 P1: code+error fields must reach the caller so the
    // banner can show the orchestration error instead of a false
    // "0 daemons restarted" success.
    expect(res.code).toBe("RESTART_FAILED");
    expect(res.error).toBe("scheduler unavailable");
  });

  it("throws on 500 SECRETS_SET_FAILED (vault not committed)", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({
        code: "SECRETS_SET_FAILED",
        error: "disk full",
      }),
    });
    await expect(rotateSecret("K1", "v", true)).rejects.toMatchObject({
      code: "SECRETS_SET_FAILED",
      status: 500,
    });
  });
});

describe("restartSecret", () => {
  it("returns body on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ restart_results: [] }),
    });
    const res = await restartSecret("K1");
    expect(res.restart_results).toEqual([]);
  });
});

describe("deleteSecret", () => {
  it("returns void on 204", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 204, json: async () => ({}) });
    await expect(deleteSecret("K1")).resolves.toBeUndefined();
  });

  it("throws with usedBy on 409 SECRETS_HAS_REFS", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({
        error: "refs",
        code: "SECRETS_HAS_REFS",
        used_by: [{ server: "alpha", env_var: "OPENAI" }],
      }),
    });
    await expect(deleteSecret("K1")).rejects.toMatchObject({
      code: "SECRETS_HAS_REFS",
      body: expect.objectContaining({ used_by: [{ server: "alpha", env_var: "OPENAI" }] }),
    });
  });

  it("sends ?confirm=true when opts.confirm", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 204, json: async () => ({}) });
    await deleteSecret("K1", { confirm: true });
    expect(mockFetch).toHaveBeenCalledWith("/api/secrets/K1?confirm=true", expect.objectContaining({ method: "DELETE" }));
  });

  it("throws with manifest_errors on 409 SECRETS_USAGE_SCAN_INCOMPLETE", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({
        error: "scan incomplete",
        code: "SECRETS_USAGE_SCAN_INCOMPLETE",
        manifest_errors: [{ path: "broken/manifest.yaml", error: "yaml: line 1" }],
      }),
    });
    await expect(deleteSecret("K1")).rejects.toMatchObject({
      code: "SECRETS_USAGE_SCAN_INCOMPLETE",
      body: expect.objectContaining({
        manifest_errors: [{ path: "broken/manifest.yaml", error: "yaml: line 1" }],
      }),
    });
  });
});
