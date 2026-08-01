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

import { postManifestCreate, postManifestValidate, getManifest, postManifestEdit, ManifestHashMismatchError, type APIError } from "./api";

describe("postManifestCreate", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("resolves with restart flags on 200 (R4-2)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ restart_required: true, hub_live: true }),
    }) as unknown as Response);
    await expect(postManifestCreate("demo", "name: demo")).resolves.toEqual({
      restartRequired: true,
      hubLive: true,
    });
  });

  it("defaults restart flags to false when the success body is empty/missing", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => {
        throw new Error("no body");
      },
    }) as unknown as Response);
    await expect(postManifestCreate("demo", "name: demo")).resolves.toEqual({
      restartRequired: false,
      hubLive: false,
    });
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
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        json: async () => ({ restart_required: false, hub_live: false }),
      } as unknown as Response;
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
    // R4-2: edit now also carries restart_required/hub_live (default false when
    // the success body omits them, as this mock does).
    expect(out).toEqual({ hash: "new-hash-abc", restartRequired: false, hubLive: false });
  });
  it("surfaces restart_required from the edit success body (R4-2)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ hash: "new-hash-xyz", restart_required: true, hub_live: true }),
    }) as unknown as Response);
    const out = await postManifestEdit("demo", "name: demo\n", "old-hash");
    expect(out).toEqual({ hash: "new-hash-xyz", restartRequired: true, hubLive: true });
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
  it("throws typed APIError on other non-2xx", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: async () => ({ error: "disk full", code: "MANIFEST_EDIT_FAILED" }),
    }) as unknown as Response);
    let err: unknown;
    try {
      await postManifestEdit("demo", "name: demo\n", "hash");
    } catch (caught) {
      err = caught;
    }
    expect(err).toBeInstanceOf(Error);
    expect((err as APIError).message).toMatch(/disk full/);
    expect((err as APIError).status).toBe(500);
    expect((err as APIError).code).toBe("MANIFEST_EDIT_FAILED");
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
    // No `warnings` in the 201 body → the absent field defaults to [].
    expect(out).toEqual({ kind: "hub-installed", name: "git", port: 9201, warnings: [] });
    expect(seen.url).toBe("/api/marketplace/install");
    expect(JSON.parse(seen.body!)).toMatchObject({ id: "git", mode: "hub" });
  });

  it("parses the 201 body's warnings into the hub-installed result", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 201,
      statusText: "Created",
      json: async () => ({
        name: "filesystem",
        port: 9301,
        mode: "hub",
        warnings: [
          "kind:global server: ${workspaceFolder} was frozen to the current working directory",
        ],
      }),
    }) as unknown as Response);
    const out = await installMarketplaceEntry({ id: "filesystem", mode: "hub" });
    expect(out).toEqual({
      kind: "hub-installed",
      name: "filesystem",
      port: 9301,
      warnings: [
        "kind:global server: ${workspaceFolder} was frozen to the current working directory",
      ],
    });
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

  // FINDING 3 regression: a 412 AVAILABILITY_PROBE_PENDING must map to its OWN
  // kind:'probe-pending' (carrying the backend reason) — NOT the 409
  // name-conflict branch. A 409 helper that branched only on HTTP code would
  // misroute the probe gate to the name-conflict retry UI; 412 keeps them
  // distinct.
  it("maps a 412 AVAILABILITY_PROBE_PENDING to kind:'probe-pending' (not name-conflict, not a throw)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 412,
      statusText: "Precondition Failed",
      json: async () => ({
        error: "server X is watch and its install-probe has not passed",
        code: "AVAILABILITY_PROBE_PENDING",
      }),
    }) as unknown as Response);
    const out = await installMarketplaceEntry({ id: "x", mode: "hub" });
    expect(out).toEqual({
      kind: "probe-pending",
      reason: "server X is watch and its install-probe has not passed",
    });
  });

  // FINDING 1 regression: a 412 carries TWO distinct gates, told apart by the
  // `code` field. REQUIRED_SECRET_MISSING must map to its OWN
  // kind:'required-secret-missing' (carrying the backend reason that names the
  // key) — NOT 'probe-pending'. A status-only branch would render the misleading
  // "host app not detected" message for an unset required secret (the Suno row's
  // main failure mode).
  it("maps a 412 REQUIRED_SECRET_MISSING to kind:'required-secret-missing' (not probe-pending)", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 412,
      statusText: "Precondition Failed",
      json: async () => ({
        error:
          "acedata_api_token is REQUIRED — the server exits on startup when it is unset",
        code: "REQUIRED_SECRET_MISSING",
      }),
    }) as unknown as Response);
    const out = await installMarketplaceEntry({ id: "suno", mode: "hub" });
    expect(out).toEqual({
      kind: "required-secret-missing",
      reason:
        "acedata_api_token is REQUIRED — the server exits on startup when it is unset",
    });
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
    expect(out).toEqual({ kind: "hub-installed", name: "git-2", port: 9202, warnings: [] });
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

import { refreshMarketplace } from "./api";

describe("refreshMarketplace", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  // FINDING 2 regression: the refresh path is a SECOND marketplace-DTO consumer
  // (beyond the initial load). It must carry the D-3 availability + probe_passes
  // fields onto the refreshed entry, or a refreshed watch / disabled-until-probe
  // row would lose its inert-gating (Catalog keys the install-button suppression
  // on them) after Refresh.
  it("carries availability + probe_passes through onto the refreshed entries", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({
        entries: [
          {
            id: "inert",
            name: "Inert",
            summary: "needs a host app",
            categories: ["dev"],
            homepage: "https://example.com",
            transport: "stdio",
            availability: "watch",
            probe_passes: false,
          },
        ],
      }),
    }) as unknown as Response);
    const { entries } = await refreshMarketplace();
    expect(entries).toHaveLength(1);
    expect(entries[0]).toMatchObject({
      id: "inert",
      availability: "watch",
      probe_passes: false,
    });
  });

  it("normalizes a missing availability to '' and omits probe_passes when absent", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({
        entries: [{ id: "ready", name: "Ready", transport: "http" }],
      }),
    }) as unknown as Response);
    const { entries } = await refreshMarketplace();
    expect(entries[0].availability).toBe("");
    // Absent probe_passes stays undefined (fail-closed grey-on-availability),
    // never coerced to a boolean that would falsely mark an inert row passing.
    expect(entries[0].probe_passes).toBeUndefined();
  });

  // S4 (bot #446 P1+P2): the refresh result carries the SEPARATE docs_only[] array,
  // and the normalizer keeps readme_url + manual_install on each pointer row (the
  // P2 refresh-DTO gap was dropping them). A pre-S4 backend omits docs_only → [].
  it("carries the docs_only[] array with readme_url + manual_install through refresh", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({
        entries: [{ id: "fs", name: "Filesystem", transport: "stdio" }],
        docs_only: [
          {
            id: "cubase",
            name: "Cubase (docs-only)",
            summary: "manual-install pointer",
            categories: ["music", "daw"],
            homepage: "https://github.com/example/cubase",
            readme_url: "https://raw.githubusercontent.com/example/cubase/main/README.md",
            manual_install: "git clone and configure virtual-MIDI",
          },
        ],
      }),
    }) as unknown as Response);
    const { entries, docsOnly } = await refreshMarketplace();
    expect(entries).toHaveLength(1);
    expect(docsOnly).toHaveLength(1);
    expect(docsOnly[0]).toMatchObject({
      id: "cubase",
      readme_url: "https://raw.githubusercontent.com/example/cubase/main/README.md",
      manual_install: "git clone and configure virtual-MIDI",
    });
  });

  it("returns an empty docs_only[] when a pre-S4 backend omits it", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({
        entries: [{ id: "fs", name: "Filesystem", transport: "stdio" }],
      }),
    }) as unknown as Response);
    const { docsOnly } = await refreshMarketplace();
    expect(docsOnly).toEqual([]);
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

import {
  acknowledgeDaemonRecoverReceipt,
  DAEMON_RECOVER_ERROR_CODES,
  getDaemonRecoverAuditLockState,
  newDaemonRecoverCorrelation,
  postDaemonRecover,
  type AuditLockSnapshot,
  type DaemonRecoverCorrelation,
  type DaemonRecoverResponse,
} from "./api";

describe("postDaemonRecover", () => {
  const correlation: DaemonRecoverCorrelation = {
    attempt_id: "11111111-1111-4111-8111-111111111111",
    occurrence_id: "22222222-2222-4222-8222-222222222222",
    server_instance: "33333333-3333-4333-8333-333333333333",
  };
  const auditLock: AuditLockSnapshot = {
    scope: "supervisor_events_log",
    server_instance: correlation.server_instance,
    revision: 7,
    state: "released",
    recovery_receipt: {
      attempt_id: correlation.attempt_id,
      occurrence_id: correlation.occurrence_id,
      server_instance: correlation.server_instance,
      task_name: "\\demo/default",
      status: "committed_success",
      lock_authorization: "none",
      termination_commit_state: "not_committed",
    },
    recovery_receipts: [],
  };

  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("posts the explicit-confirmation recovery contract", async () => {
    const terminationUnconfirmed: DaemonRecoverResponse["port_owner_check"] =
      "termination_unconfirmed";
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({
        task_name: "demo/default",
        state: "respawn_accepted",
        reaped: false,
        port_owner_check: terminationUnconfirmed,
        port_wait_outcome: "still_bound",
        audit_handoff: "durable",
        termination_committed: true,
        audit_lock: auditLock,
      }),
    }) as unknown as Response);

    await expect(postDaemonRecover("demo/default", correlation)).resolves.toMatchObject({
      state: "respawn_accepted",
      reaped: false,
      port_owner_check: "termination_unconfirmed",
      audit_handoff: "durable",
    });
    expect(globalThis.fetch).toHaveBeenCalledWith("/api/daemon/recover", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        task_name: "demo/default",
        confirm: true,
        audit_lock_attempt: correlation,
      }),
    });
  });

  it("preserves the stable backend reason code for safe UI copy", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 409,
      statusText: "Conflict",
      json: async () => ({
        error: "redacted operation failed",
        code: "RECOVER_REFUSED_PORT_OWNER",
      }),
    }) as unknown as Response);

    let err: APIError | undefined;
    try {
      await postDaemonRecover("demo/default", correlation);
    } catch (cause) {
      err = cause as APIError;
    }
    if (!err) throw new Error("expected recovery request to fail");
    expect(err.code).toBe("RECOVER_REFUSED_PORT_OWNER");
    expect(err.status).toBe(409);
  });

  it("pins the complete stable backend error-code contract", () => {
    expect(DAEMON_RECOVER_ERROR_CODES).toEqual([
      "INVALID_ARGS",
      "RECOVER_CONFIRMATION_REQUIRED",
      "RECOVER_UNKNOWN_TASK",
      "RECOVER_REFUSED_PORT_OWNER",
      "RECOVER_RESPAWN_FAILED",
      "RECOVER_SUPERVISOR_UNAVAILABLE",
      "RECOVER_REQUEST_CANCELED",
      "RECOVER_BOUNDARY_PROBE_TIMEOUT",
      "RECOVER_RESPAWN_BUDGET_INSUFFICIENT",
      "RECOVER_STATE_READ_FAILED",
      "RECOVER_AUDIT_DURABILITY_FAILED",
      "RECOVER_UNCLASSIFIED_FAILURE",
      "AUDIT_LOCK_ADAPTER_INIT_FAILED",
      "RECOVER_CORRELATION_INVALID",
      "RECOVER_BASELINE_STALE",
      "RECOVER_ATTEMPT_CONFLICT",
      "RECOVER_OCCURRENCE_CONSUMED",
      "RECOVER_OCCURRENCE_CAPACITY_EXCEEDED",
      "RECOVER_RECEIPT_IN_FLIGHT",
      "RECOVER_OUTCOME_UNCERTAIN",
    ]);
  });

  // A stranded event-log flock is reported on the SUCCESS response, so the UI
  // must be able to read it without treating the recovery as failed.
  it("carries the audit-handoff warning on a successful recovery", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({
        task_name: "demo/default",
        state: "respawn_accepted",
        reaped: true,
        port_owner_check: "reaped",
        port_wait_outcome: "released",
        audit_handoff: "release_unconfirmed",
        termination_committed: true,
        audit_lock: { ...auditLock, state: "stranded", revision: 8 },
      }),
    }) as unknown as Response);

    const resolved = await postDaemonRecover("demo/default", correlation);
    if (resolved.state === "recovery_in_flight") {
      throw new Error("unexpected in-flight replay");
    }
    const handoff: DaemonRecoverResponse["audit_handoff"] =
      resolved.audit_handoff;
    expect(handoff).toBe("release_unconfirmed");
    expect(resolved.state).toBe("respawn_accepted");
  });

  it("creates canonical lowercase UUIDv4 identifiers", () => {
    const created = newDaemonRecoverCorrelation(correlation.server_instance);
    const canonicalV4 =
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
    expect(created.server_instance).toBe(correlation.server_instance);
    expect(created.attempt_id).toMatch(canonicalV4);
    expect(created.occurrence_id).toMatch(canonicalV4);
    expect(created.attempt_id).not.toBe(created.occurrence_id);
  });

  it("fails closed when Web Crypto is unavailable", () => {
    vi.stubGlobal("crypto", undefined);
    try {
      expect(() =>
        newDaemonRecoverCorrelation(correlation.server_instance)
      ).toThrow("Web Crypto is unavailable; recovery was not started.");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("looks up and acknowledges the exact full correlation tuple", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        statusText: "OK",
        json: async () => auditLock,
      } as unknown as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 204,
        statusText: "No Content",
        json: async () => null,
      } as unknown as Response);
    globalThis.fetch = fetchMock;

    const controller = new AbortController();
    await expect(
      getDaemonRecoverAuditLockState(correlation, controller.signal),
    ).resolves.toEqual(auditLock);
    await expect(acknowledgeDaemonRecoverReceipt(correlation)).resolves.toBeUndefined();

    const lookupURL = fetchMock.mock.calls[0]?.[0]?.toString() ?? "";
    expect(lookupURL).toContain("attempt_id=11111111-1111-4111-8111-111111111111");
    expect(lookupURL).toContain("occurrence_id=22222222-2222-4222-8222-222222222222");
    expect(lookupURL).toContain("server_instance=33333333-3333-4333-8333-333333333333");
    expect(fetchMock.mock.calls[0]?.[1]).toEqual({ signal: controller.signal });
    expect(fetchMock.mock.calls[1]).toEqual([
      "/api/daemon/recover/audit-lock-receipt",
      {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...correlation, acknowledge: true }),
      },
    ]);
  });
});
