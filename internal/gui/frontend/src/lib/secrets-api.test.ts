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
  const capability = { schema_version: 1, workflow: "cli_support_readonly", path_free_errors: true };
  function reply(status: number, body: unknown) {
    return { ok: status >= 200 && status < 300, status, json: async () => body };
  }
  function initReply(status: number, body: unknown) {
    mockFetch.mockResolvedValueOnce(reply(200, capability)).mockResolvedValueOnce(reply(status, body));
  }

  it("returns the successful body after a fresh capability check", async () => {
    initReply(200, { vault_state: "ok" });
    expect((await secretsInit()).vault_state).toBe("ok");
    expect(mockFetch.mock.calls.map(c => c[0])).toEqual(["/api/secrets/init-capabilities", "/api/secrets/init"]);
  });

  it.each([undefined, null, {}, { schema_version: 0 }, { ...capability, schema_version: 2 },
    { ...capability, workflow: "other" }, { ...capability, path_free_errors: false },
    { ...capability, path_free_errors: "true" }, { ...capability, extra: true }])(
    "blocks unsupported or malformed capability %j without a POST", async body => {
      mockFetch.mockResolvedValue(reply(200, body));
      await expect(secretsInit()).rejects.toMatchObject({ code: "SECRETS_INIT_UNSUPPORTED" });
      expect(mockFetch.mock.calls.every(c => c[1]?.method !== "POST")).toBe(true);
    });

  it("blocks a legacy missing capability endpoint and raw network failure", async () => {
    // Historical producer RED: HTTP405, text/plain "method not allowed".
    mockFetch.mockResolvedValue({ ok: false, status: 405, json: async () => { throw new SyntaxError("method not allowed"); } });
    await expect(secretsInit()).rejects.toMatchObject({ code: "SECRETS_INIT_UNSUPPORTED" });
    mockFetch.mockReset().mockRejectedValue(new Error("private-marker"));
    await expect(secretsInit()).rejects.toThrow("Reload or update the application");
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it.each([
    [409, { code: "SECRETS_INIT_BLOCKED", error: "private-marker" }],
    [200, { vault_state: "missing", cleanup_status: "ok", code: "SECRETS_INIT_FAILED", error: "private-marker", recovery_id: "216980ce5b66faa0cc5363401ea67007", recovery_status: "ready" }],
    [500, { cleanup_status: "failed", code: "SECRETS_INIT_FAILED", error: "private-marker", orphan_path: "private-orphan", recovery_id: "6a17532f94e218f277f8f94801cb67f8", recovery_status: "ready" }],
    [500, { code: "SECRETS_INIT_FAILED", error: "private-marker", recovery_id: "f3c3da1270290f0451de4bfc89fa3a84", recovery_status: "incomplete" }],
    [500, { code: "SECRETS_INIT_FAILED", error: "private-marker", recovery_status: "unavailable" }],
  ])("preserves status/body/code but contains the visible message (%i)", async (status, body) => {
    initReply(status as number, body);
    const err = await secretsInit().catch(e => e);
    expect(err).toMatchObject({ status, code: (body as {code: string}).code, body });
    expect(err.message).not.toContain("private-marker");
    expect(err.message).not.toContain("private-orphan");
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });

  it("does not promote unsafe references from a successful body", async () => {
    const body = { vault_state: "ok", recovery_id: "private-marker", recovery_status: "ready" };
    initReply(200, body);
    const result = await secretsInit();
    expect(result.body).toEqual(body);
    expect(result.status).toBe(200);
    expect(result.recovery_id).toBeUndefined();
  });

  it.each([null, {}, { vault_state: "missing" }, { vault_state: "ok", error: "private-marker" }])(
    "does not grant success to a missing or contradictory HTTP200 body %j", async body => {
      initReply(200, body);
      await expect(secretsInit()).rejects.toMatchObject({ status: 200, body });
    });

  it("rechecks capability for each caller after a server change", async () => {
    initReply(200, { vault_state: "ok" });
    await secretsInit();
    mockFetch.mockResolvedValueOnce(reply(200, { ...capability, path_free_errors: false }));
    await expect(secretsInit()).rejects.toMatchObject({ code: "SECRETS_INIT_UNSUPPORTED" });
    expect(mockFetch.mock.calls.filter(c => c[1]?.method === "POST")).toHaveLength(1);
  });
  it.each(["", "0".repeat(32), "A".repeat(32), "../private", "1".repeat(31)])(
    "does not promote an unsafe support reference %s", async recovery_id => {
      initReply(500, { code: "SECRETS_INIT_FAILED", recovery_id, recovery_status: "ready" });
      const err = await secretsInit().catch(e => e);
      expect(err.recovery_id).toBeUndefined();
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
