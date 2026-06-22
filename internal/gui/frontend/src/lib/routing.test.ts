import { describe, expect, it } from "vitest";
import {
  isHubLoopback,
  isSerenaRouterURL,
  loopbackEntryPort,
  loopbackPortMatchesDaemon,
  perClientRouting,
  collectServers,
  visibleClients,
  remoteHTTPCapableClients,
  orderClientsForColumns,
  CORE_CLIENTS,
  NON_CORE_CLIENTS,
  ALL_CLIENTS,
} from "./routing";
import type { ClientCapability, ScanEntry, ScanResult } from "../types";

describe("isHubLoopback", () => {
  it("accepts 127.0.0.1 URLs", () => {
    expect(isHubLoopback("http://127.0.0.1:9123/mcp")).toBe(true);
  });
  it("accepts localhost URLs", () => {
    expect(isHubLoopback("http://localhost:9123/mcp")).toBe(true);
  });
  it("accepts [::1] URLs", () => {
    expect(isHubLoopback("http://[::1]:9123/mcp")).toBe(true);
  });
  it("rejects subdomain-as-path spoofs like 127.0.0.1.evil.com", () => {
    expect(isHubLoopback("https://127.0.0.1.evil.com/foo")).toBe(false);
  });
  it("rejects query-param spoofs like ?host=127.0.0.1", () => {
    expect(isHubLoopback("https://example.com/?host=127.0.0.1")).toBe(false);
  });
  it("rejects non-http schemes like stdio://", () => {
    expect(isHubLoopback("stdio:///memory")).toBe(false);
  });
  it("rejects empty string", () => {
    expect(isHubLoopback("")).toBe(false);
  });
});

describe("isSerenaRouterURL", () => {
  it("accepts loopback /serena/mcp router URLs on any port", () => {
    expect(isSerenaRouterURL("http://127.0.0.1:9125/serena/mcp")).toBe(true);
    expect(isSerenaRouterURL("http://localhost:9130/serena/mcp")).toBe(true);
  });

  it("rejects legacy /mcp paths and non-loopback hosts", () => {
    expect(isSerenaRouterURL("http://127.0.0.1:9121/mcp")).toBe(false);
    expect(isSerenaRouterURL("https://example.com/serena/mcp")).toBe(false);
  });
});

describe("loopbackEntryPort", () => {
  it("extracts the port from a loopback hub URL", () => {
    expect(loopbackEntryPort("http://127.0.0.1:9133/mcp")).toBe(9133);
    expect(loopbackEntryPort("http://localhost:9123/mcp")).toBe(9123);
    expect(loopbackEntryPort("http://[::1]:9128/mcp")).toBe(9128);
  });
  it("returns null for a loopback URL with no explicit port", () => {
    expect(loopbackEntryPort("http://localhost/mcp")).toBeNull();
  });
  it("returns null for a non-loopback URL", () => {
    expect(loopbackEntryPort("https://mcp.context7.com:443/mcp")).toBeNull();
  });
  it("returns null for an unparseable endpoint", () => {
    expect(loopbackEntryPort("stdio:///memory")).toBeNull();
    expect(loopbackEntryPort("")).toBeNull();
  });
});

describe("loopbackPortMatchesDaemon", () => {
  it("matches when the loopback port is a declared daemon port", () => {
    expect(loopbackPortMatchesDaemon("http://localhost:9133/mcp", [9133])).toBe(true);
    expect(loopbackPortMatchesDaemon("http://localhost:9302/mcp", [9301, 9302])).toBe(true);
  });
  it("does NOT match a stale/foreign port", () => {
    // fetch pointed at serena's 9121 when fetch's daemon is 9133.
    expect(loopbackPortMatchesDaemon("http://localhost:9121/mcp", [9133])).toBe(false);
  });
  it("does NOT match when there are no daemon ports", () => {
    expect(loopbackPortMatchesDaemon("http://localhost:9133/mcp", [])).toBe(false);
  });
  it("does NOT match a non-loopback URL even on port collision", () => {
    expect(loopbackPortMatchesDaemon("https://evil.com:9133/mcp", [9133])).toBe(false);
  });
});

describe("perClientRouting", () => {
  it("tags hub loopback http at a MATCHING daemon port as via-hub", () => {
    const r = perClientRouting(
      { "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9100/mcp" } },
      {},
      true,
      "memory",
      [9100],
    );
    expect(r["claude-code"]).toBe("via-hub");
  });
  it("tags hub loopback http at a NON-matching (stale) port as direct", () => {
    // PORT-AWARE FIX (security review): loopback shape alone is not enough.
    // A stale-port loopback entry must NOT render as a green via-hub cell.
    const r = perClientRouting(
      { "claude-code": { transport: "http", endpoint: "http://localhost:9121/mcp" } },
      {},
      true,
      "fetch",
      [9133],
    );
    expect(r["claude-code"]).toBe("direct");
  });
  it("serena router http is via-hub when guiPort is unknown (degrade, port-agnostic)", () => {
    const r = perClientRouting(
      { "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9125/serena/mcp" } },
      {},
      true,
      "serena",
      [9121],
      0, // unknown live port → degrade to port-agnostic
    );
    expect(r["claude-code"]).toBe("via-hub");
  });
  it("serena router http is via-hub on the LIVE gui port", () => {
    const r = perClientRouting(
      { "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9125/serena/mcp" } },
      {},
      true,
      "serena",
      [9121],
      9125, // live gui port matches the cell's port
    );
    expect(r["claude-code"]).toBe("via-hub");
  });
  it("serena router http on a STALE port is direct (matches backend external)", () => {
    const r = perClientRouting(
      { "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9124/serena/mcp" } },
      {},
      true,
      "serena",
      [9121],
      9125, // gui re-bound to 9125; the 9124 entry is stale → not managed
    );
    expect(r["claude-code"]).toBe("direct");
  });
  it("tags loopback http as direct when the server has no daemon ports", () => {
    const r = perClientRouting(
      { "claude-code": { transport: "http", endpoint: "http://localhost:7777/mcp" } },
      {},
      true,
      "my-own-local-server",
      [],
    );
    expect(r["claude-code"]).toBe("direct");
  });
  it("tags relay transport as via-hub (port check does not apply to relay)", () => {
    const r = perClientRouting({ "codex-cli": { transport: "relay" } });
    expect(r["codex-cli"]).toBe("via-hub");
  });
  it("serena relay router is via-hub on the LIVE gui port", () => {
    const r = perClientRouting(
      {
        antigravity: {
          transport: "relay",
          relay_url: "http://127.0.0.1:9125/serena/mcp",
        },
      },
      {},
      true,
      "serena",
      [9121],
      9125,
    );
    expect(r.antigravity).toBe("via-hub");
  });
  it("serena relay router on a STALE port is direct", () => {
    const r = perClientRouting(
      {
        antigravity: {
          transport: "relay",
          relay_url: "http://127.0.0.1:9124/serena/mcp",
        },
      },
      {},
      true,
      "serena",
      [9121],
      9125,
    );
    expect(r.antigravity).toBe("direct");
  });
  it("serena relay without resolved relay_url is direct", () => {
    const r = perClientRouting(
      { antigravity: { transport: "relay" } },
      {},
      true,
      "serena",
      [9121],
      9125,
    );
    expect(r.antigravity).toBe("direct");
  });
  it("tags remote http as direct", () => {
    const r = perClientRouting({
      "gemini-cli": { transport: "http", endpoint: "https://example.com/mcp" },
    });
    expect(r["gemini-cli"]).toBe("direct");
  });
  it("tags stdio as direct", () => {
    const r = perClientRouting({ "antigravity": { transport: "stdio" } });
    expect(r["antigravity"]).toBe("direct");
  });
  it("tags absent transport as not-installed", () => {
    const r = perClientRouting({ "claude-code": { transport: "absent" } });
    expect(r["claude-code"]).toBe("not-installed");
  });
  it("tags missing transport as not-installed", () => {
    const r = perClientRouting({ "claude-code": {} });
    expect(r["claude-code"]).toBe("not-installed");
  });
});

describe("collectServers", () => {
  it("sorts by name ascending", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        { name: "zulu", client_presence: {} },
        { name: "alpha", client_presence: {} },
      ],
    };
    const out = collectServers(scan);
    expect(out.map((s) => s.name)).toEqual(["alpha", "zulu"]);
  });
  it("handles null entries gracefully", () => {
    const out = collectServers({ at: "", entries: null });
    expect(out).toEqual([]);
  });
  it("derives routing from client_presence (port matches daemon)", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        {
          name: "serena",
          client_presence: {
            "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9100/mcp" },
          },
          daemon_ports: [9100],
        },
      ],
    };
    const out = collectServers(scan);
    expect(out[0].routing["claude-code"]).toBe("via-hub");
  });

  it("renders a serena router cell as via-hub even when daemon_ports is legacy 9121", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        {
          name: "serena",
          client_presence: {
            "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9125/serena/mcp" },
          },
          daemon_ports: [9121],
        },
      ],
    };
    const out = collectServers(scan);
    expect(out[0].routing["claude-code"]).toBe("via-hub");
  });

  // PORT-AWARE FIX (security review): collectServers threads
  // ScanEntry.daemon_ports into perClientRouting so a stale-port loopback
  // entry renders "direct" (unmanaged), matching the backend status —
  // not a deceptive green via-hub cell.
  it("renders a stale-port loopback cell as direct, not via-hub", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        {
          name: "fetch",
          client_presence: {
            // points at serena's 9121, but fetch's daemon is 9133
            "gemini-cli": { transport: "http", endpoint: "http://localhost:9121/mcp" },
            // correctly migrated cell at fetch's own daemon port
            "qwen-cli": { transport: "http", endpoint: "http://localhost:9133/mcp" },
          },
          daemon_ports: [9133],
        },
      ],
    };
    const out = collectServers(scan);
    expect(out[0].routing["gemini-cli"]).toBe("direct");
    expect(out[0].routing["qwen-cli"]).toBe("via-hub");
  });

  // Bug-bash A3 (#11/#12): each ServerRow carries a `manifested` flag
  // so Servers.tsx can split mcphub-managed rows from legacy non-mcphub
  // entries discovered via /api/scan. Pre-fix, both groups rendered in
  // the same matrix and the legacy rows had live but no-op checkboxes.
  it("threads manifest_exists onto ServerRow.manifested", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        { name: "serena", client_presence: {}, manifest_exists: true },
        { name: "time-server", client_presence: {}, manifest_exists: false },
        { name: "ambiguous", client_presence: {} }, // omitted
      ],
    };
    const out = collectServers(scan);
    const byName = Object.fromEntries(out.map((s) => [s.name, s]));
    expect(byName["serena"].manifested).toBe(true);
    expect(byName["time-server"].manifested).toBe(false);
    // Default for omitted: not manifested (safer than treating unknown
    // as managed, since that lets the matrix render an Apply for
    // something that has no plan).
    expect(byName["ambiguous"].manifested).toBe(false);
  });
});

// Bug-bash A2 (#13) closure: client_config_presence drives "available"
// vs "not-installed" for clients absent from per-entry client_presence.
// Without this, a client with `mcpServers: {}` was indistinguishable
// from "client not on this host" and the whole column was disabled.
describe("perClientRouting with client_config_presence", () => {
  it("tags missing-from-presence + config 'ok' as available", () => {
    const r = perClientRouting({}, { "claude-code": "ok" });
    expect(r["claude-code"]).toBe("available");
  });

  it("tags missing-from-presence + config 'missing' as not-installed", () => {
    const r = perClientRouting({}, { "claude-code": "missing" });
    expect(r["claude-code"]).toBe("not-installed");
  });

  it("tags missing-from-presence + config 'error' as config-error (deep-sec Lane B)", () => {
    const r = perClientRouting({}, { "claude-code": "error" });
    expect(r["claude-code"]).toBe("config-error");
  });

  it("tags missing-from-presence + config 'error-symlink' as config-error-symlink (2026-05-19 message-accuracy fix)", () => {
    // A symlinked client config is refused by the secure-write pipeline
    // in all modes (PR #209). It must map to its OWN routing tag so the
    // matrix renders the symlink-specific tooltip, NOT the generic
    // stat-error one.
    const r = perClientRouting({}, { "codex-cli": "error-symlink" });
    expect(r["codex-cli"]).toBe("config-error-symlink");
    expect(r["codex-cli"]).not.toBe("config-error");
  });

  it("does NOT override an existing per-entry signal with config presence", () => {
    const r = perClientRouting(
      { "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9100/mcp" } },
      { "claude-code": "ok" },
      true,
      "memory",
      [9100],
    );
    expect(r["claude-code"]).toBe("via-hub");
  });

  // Finding 4 (mimocode): a present-but-DISABLED (enabled:false) entry is
  // recorded as an "absent" transport in client_presence. The FIRST pass must
  // win and tag it "not-installed" (unchecked + DISABLED, not a migrate
  // candidate) — the SECOND pass must NOT promote it to "available" off
  // config_presence:"ok", which would let Apply CLOBBER the disabled user
  // entry. This is the routing half of the scan-side absent-presence fix.
  it("absent presence + config 'ok' stays not-installed, never available (Finding 4 clobber-protection)", () => {
    const r = perClientRouting(
      { mimocode: { transport: "absent" } },
      { mimocode: "ok" },
      true,
      "memory",
    );
    expect(r["mimocode"]).toBe("not-installed");
    expect(r["mimocode"]).not.toBe("available");
  });

  it("fills every known client column even with empty client_presence", () => {
    const r = perClientRouting({}, {});
    for (const c of [
      "claude-code",
      "codex-cli",
      "cursor",
      "vscode",
      "gemini-cli",
      "qwen-cli",
      "antigravity",
    ]) {
      expect(r[c]).toBe("not-installed");
    }
  });

  it("classifies EVERY ALL_CLIENTS member, not just the core seven", () => {
    // The routing second pass must cover the full registry mirror so a
    // detected non-core client (any of the 46) gets a correctly classified
    // cell, not a missing key. Pre-fix only the 15 hardcoded clients were
    // classified.
    const r = perClientRouting({}, {});
    for (const c of ALL_CLIENTS) {
      expect(r[c]).toBe("not-installed");
    }
  });

  it("classifies a NEWER non-core client (warp) as available when its config is ok", () => {
    const r = perClientRouting({}, { warp: "ok" }, true);
    expect(r["warp"]).toBe("available");
  });

  it("classifies a detected client NOT in ALL_CLIENTS (drift-resilient via presence keys)", () => {
    // A backend client newer than the static list still gets a classified
    // cell because the routing universe unions the presence-map keys.
    const r = perClientRouting({}, { "future-client-xyz": "ok" }, true);
    expect(r["future-client-xyz"]).toBe("available");
  });

  it("collectServers threads client_config_presence to perClientRouting", () => {
    const scan: ScanResult = {
      at: "",
      entries: [{ name: "godbolt", client_presence: {}, can_migrate: true }],
      client_config_presence: { "claude-code": "ok", vscode: "missing" },
    };
    const out = collectServers(scan);
    expect(out[0].routing["claude-code"]).toBe("available");
    expect(out[0].routing["vscode"]).toBe("not-installed");
  });

  // Bot r1 P2 closure: non-migratable servers (no manifest,
  // per-session, unknown) must NOT get "available" cells. Pre-fix the
  // fallback enabled checkboxes for them which Apply could not honor.
  it("does NOT mark cells 'available' when server is not migratable", () => {
    const r = perClientRouting({}, { "claude-code": "ok" }, false);
    expect(r["claude-code"]).toBe("not-installed");
  });

  it("collectServers honors can_migrate=false (gates 'available' fallback)", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        { name: "manifested", client_presence: {}, can_migrate: true },
        { name: "time-server", client_presence: {}, can_migrate: false },
        { name: "unknown-default", client_presence: {} }, // undefined can_migrate → treat as non-migratable
      ],
      client_config_presence: { "claude-code": "ok" },
    };
    const out = collectServers(scan);
    const byName = Object.fromEntries(out.map((s) => [s.name, s]));
    expect(byName["manifested"].routing["claude-code"]).toBe("available");
    expect(byName["time-server"].routing["claude-code"]).toBe("not-installed");
    expect(byName["unknown-default"].routing["claude-code"]).toBe("not-installed");
  });

  // v0.4.5 init-button: "missing-init-possible" maps to "not-installed"
  // at the per-cell routing level (cells stay disabled), but the
  // separate per-column header affordance picks up the same state to
  // render the Initialize button. The routing test below pins the
  // first half of that contract — the header behavior is tested
  // separately through the ServersScreen test.
  it("tags missing-init-possible as not-installed at the cell level", () => {
    const r = perClientRouting({}, { "claude-code": "missing-init-possible" });
    expect(r["claude-code"]).toBe("not-installed");
  });

  it("tags missing-init-possible as not-installed even when can_migrate=true", () => {
    const r = perClientRouting({}, { "claude-code": "missing-init-possible" }, true);
    expect(r["claude-code"]).toBe("not-installed");
  });

  // v0.4.5 PR #208 codex r1 F2: "missing-init-blocked-symlink" also
  // maps to "not-installed" at the cell level. The matrix header
  // suppresses the Initialize button for this state (Servers.tsx
  // gates the button on === "missing-init-possible" strictly), so
  // a symlinked-parent client renders as a disabled column with no
  // Initialize affordance.
  it("tags missing-init-blocked-symlink as not-installed at the cell level", () => {
    const r = perClientRouting({}, { "claude-code": "missing-init-blocked-symlink" });
    expect(r["claude-code"]).toBe("not-installed");
  });

  it("does not classify missing-init-blocked-symlink as config-error", () => {
    // config-error is reserved for non-IsNotExist stat failures on the
    // config file itself (permissions, ACL anomaly). A symlinked parent
    // is a different category — the operator's setup is valid, the
    // pipeline just won't follow it.
    const r = perClientRouting({}, { "claude-code": "missing-init-blocked-symlink" });
    expect(r["claude-code"]).not.toBe("config-error");
  });
});

// The matrix is too wide to show every column always, so the non-core
// opt-in clients are detection-gated while the seven core clients always
// render. visibleClients() decides the columns. The non-core candidate
// universe is derived from the scan's client_config_presence map (one key
// per clients.SupportedClientNames()), NOT a frontend-hardcoded list, so all
// 46 backend clients can surface when detected — and a backend client newer
// than the frontend NON_CORE_CLIENTS list still surfaces when detected.
describe("visibleClients (scannable + file-present non-core columns)", () => {
  // capsFor builds a client_capabilities map marking the given client ids as
  // scannable (the matrix only shows a non-core column for a SCANNABLE client
  // — one with a backend clientScanners() parser). Defaults to marking every
  // CORE + NON_CORE client scannable, plus any presence/entry key, so the
  // generic "ok"-detection tests below behave as before; the dedicated
  // non-scannable-exclusion test overrides this with an explicit empty/partial
  // map.
  function capsFor(
    ids: Iterable<string>,
  ): ScanResult["client_capabilities"] {
    const out: NonNullable<ScanResult["client_capabilities"]> = {};
    for (const id of ids) {
      // These visibility tests gate on `scannable` only; direct_installable /
      // remote_http_capable are irrelevant here, so default both to false.
      out[id] = { scannable: true, direct_installable: false, remote_http_capable: false };
    }
    return out;
  }

  function scan(
    presence: Record<string, string>,
    entries: ScanEntry[] = [],
    capabilities?: ScanResult["client_capabilities"],
  ): ScanResult {
    // Default capabilities: everything the test names is scannable, so the
    // file-present gate is what is under test. A test exercising the
    // non-scannable exclusion passes an explicit (narrower) capabilities map.
    const referenced = new Set<string>();
    for (const e of entries) {
      for (const c of Object.keys(e.client_presence ?? {})) referenced.add(c);
    }
    const caps =
      capabilities ??
      capsFor([
        ...CORE_CLIENTS,
        ...NON_CORE_CLIENTS,
        ...Object.keys(presence),
        ...referenced,
      ]);
    return {
      at: "",
      entries,
      client_config_presence: presence as ScanResult["client_config_presence"],
      client_capabilities: caps,
    } as ScanResult;
  }

  it("always shows the seven core clients, even on a bare host", () => {
    const cols = visibleClients(scan({}));
    for (const c of CORE_CLIENTS) {
      expect(cols).toContain(c);
    }
    // No non-core client detected → none appended → exactly the core set.
    expect(cols).toHaveLength(CORE_CLIENTS.length);
  });

  it("never shows a non-core client that is merely present in the presence map as 'missing'", () => {
    // The presence map carries ALL 46 backend clients, most as plain
    // "missing" on a typical host. Those must NOT become columns — only a
    // DETECTED state (or a referencing entry) gates a column. This is the
    // core anti-overflow guarantee with the now-46-wide candidate universe.
    const allMissing: Record<string, string> = {};
    for (const c of NON_CORE_CLIENTS) allMissing[c] = "missing";
    const cols = visibleClients(scan(allMissing));
    expect(cols).toEqual([...CORE_CLIENTS]);
    expect(cols).toHaveLength(CORE_CLIENTS.length);
  });

  it("hides an undetected non-core client (plain 'missing' or absent)", () => {
    const cols = visibleClients(scan({ zed: "missing", kiro: "missing" }));
    expect(cols).not.toContain("zed");
    expect(cols).not.toContain("kiro");
  });

  it("shows a non-core client whose config file is present ('ok')", () => {
    const cols = visibleClients(scan({ zed: "ok" }));
    expect(cols).toContain("zed");
    // Other non-core clients stay hidden.
    expect(cols).not.toContain("kiro");
  });

  it("shows a NEWER non-core client (beyond the original wave-2 set) when its config FILE is present", () => {
    // A scannable non-core client with an actual config file ("ok") surfaces.
    // A scannable client whose file is merely creatable ("missing-init-*")
    // does NOT — that is the anti-overflow gate (Finding 1): on a fresh
    // profile the absent-but-creatable config dirs must not manufacture
    // columns. (warp/goose are non-scannable in production, but the generic
    // scan() helper marks them scannable here so this test isolates the
    // FILE-PRESENT gate; cline/zed are used as scannable file-present cases.)
    const cols = visibleClients(scan({ cline: "ok", zed: "missing-init-possible", kilocode: "ok" }));
    expect(cols).toContain("cline");
    expect(cols).toContain("kilocode");
    // zed's parent dir exists but its config file is absent → no column.
    expect(cols).not.toContain("zed");
  });

  it("shows every SCANNABLE non-core client that is file-present (full scannable universe)", () => {
    // When every non-core client is 'ok' AND every one is scannable (the
    // generic helper marks them so), every one becomes a column. This is the
    // upper-bound regression guard for the reported bug — pre-fix only ~15
    // hardcoded clients could ever appear regardless of detection.
    const allOk: Record<string, string> = {};
    for (const c of NON_CORE_CLIENTS) allOk[c] = "ok";
    const cols = visibleClients(scan(allOk));
    expect(cols).toEqual([...ALL_CLIENTS]);
    expect(cols).toHaveLength(ALL_CLIENTS.length);
  });

  it("HIDES a file-present non-core client that is NOT scannable (no clientScanners parser)", () => {
    // Finding 3: a presence-probed-but-unparsed client (copilot-cli/amazon-q/
    // openhands/aider in production) gets no enabled column even with an 'ok'
    // config file — /api/scan can never report its per-entry presence, so the
    // cell could never be reconciled/demigrated after a migrate. Here 'aider'
    // is file-present but NOT in the scannable capability map, while 'zed' is
    // both scannable and file-present.
    // This test gates on `scannable` only; direct_installable / remote_http_capable
    // are irrelevant here, so default both to false.
    const caps: NonNullable<ScanResult["client_capabilities"]> = {};
    for (const c of CORE_CLIENTS)
      caps[c] = { scannable: true, direct_installable: false, remote_http_capable: false };
    caps.zed = { scannable: true, direct_installable: false, remote_http_capable: false };
    caps.aider = { scannable: false, direct_installable: false, remote_http_capable: false };
    const cols = visibleClients(
      scan({ zed: "ok", aider: "ok" }, [], caps),
    );
    expect(cols).toContain("zed");
    expect(cols).not.toContain("aider");
  });

  it("shows ONLY the core set on a fresh profile (no overflow), even with the full presence map", () => {
    // The anti-overflow guarantee end-to-end: a fresh profile reports every
    // non-core client as 'missing' or 'missing-init-*' (no real config file),
    // so NONE earn a column regardless of scannability — exactly the core set.
    const fresh: Record<string, string> = {};
    let i = 0;
    for (const c of NON_CORE_CLIENTS) {
      // alternate the absent-but-creatable states to prove none counts as
      // detection for a non-core column.
      fresh[c] = i % 2 === 0 ? "missing" : "missing-init-possible";
      i++;
    }
    const cols = visibleClients(scan(fresh));
    expect(cols).toEqual([...CORE_CLIENTS]);
    expect(cols).toHaveLength(CORE_CLIENTS.length);
  });

  it("surfaces a detected backend client NOT in the static NON_CORE_CLIENTS list (drift-resilient)", () => {
    // A backend client newer than this frontend list must still appear when
    // detected, because the candidate universe is the presence map, not the
    // static list. It sorts AFTER all known non-core clients (extras last).
    const cols = visibleClients(scan({ "future-client-xyz": "ok" }));
    expect(cols).toContain("future-client-xyz");
    // Core stays first; the unknown client is the tail.
    expect(cols.slice(0, CORE_CLIENTS.length)).toEqual([...CORE_CLIENTS]);
    expect(cols[cols.length - 1]).toBe("future-client-xyz");
  });

  it("HIDES a non-core client whose config FILE is absent but parent dir exists ('missing-init-possible')", () => {
    // Finding 1: parent-dir-exists / file-absent is NOT detection for a
    // derived non-core column. On a fresh profile dozens of niche-client
    // config dirs would otherwise overflow the matrix. A non-core client
    // earns a column only when an actual config FILE exists.
    const cols = visibleClients(scan({ windsurf: "missing-init-possible" }));
    expect(cols).not.toContain("windsurf");
  });

  it("HIDES a non-core client whose config dir is absent-but-creatable ('missing-init-creatable')", () => {
    // Same anti-overflow gate (Finding 1): "missing-init-creatable" means the
    // file AND its parent are absent (just securely creatable). On a CLEAN
    // install that is the overflow case — every niche client would show. The
    // clean-install Initialize guarantee G17 added is a CORE-client guarantee
    // (core columns are always shown and matrix-stable); a NON-core client
    // must have a real config file before it earns a column.
    const cols = visibleClients(scan({ kiro: "missing-init-creatable" }));
    expect(cols).not.toContain("kiro");
    // No non-core client is file-present here → exactly the core set.
    expect(cols).toEqual([...CORE_CLIENTS]);
  });

  it("shows a non-core client in an error state (config present but unreadable)", () => {
    const cols = visibleClients(scan({ hermes: "error", openclaw: "error-symlink" }));
    expect(cols).toContain("hermes");
    expect(cols).toContain("openclaw");
  });

  it("shows a non-core client that already has a server entry, even if presence is absent", () => {
    const entries: ScanEntry[] = [
      {
        name: "memory",
        client_presence: { cline: { transport: "http", endpoint: "http://127.0.0.1:9123/mcp" } },
      } as ScanEntry,
    ];
    const cols = visibleClients(scan({}, entries));
    expect(cols).toContain("cline");
  });

  it("appends detected non-core clients after the core set in stable registry order", () => {
    // Detect three non-core clients out of presence-map order; expect
    // core-then-non-core order (non-core in NON_CORE_CLIENTS / registry
    // declaration order, not presence-map order).
    const cols = visibleClients(scan({ openclaw: "ok", zed: "ok", cline: "ok" }));
    expect(cols.slice(0, CORE_CLIENTS.length)).toEqual([...CORE_CLIENTS]);
    const tail = cols.slice(CORE_CLIENTS.length);
    // Stable order = filtered NON_CORE_CLIENTS order: zed < cline < openclaw.
    expect(tail).toEqual(
      NON_CORE_CLIENTS.filter((c) => c === "zed" || c === "cline" || c === "openclaw"),
    );
  });

  it("tolerates a null/undefined scan", () => {
    expect(visibleClients(null)).toEqual([...CORE_CLIENTS]);
    expect(visibleClients(undefined)).toEqual([...CORE_CLIENTS]);
  });
});

// remoteHTTPCapableClients derives the Catalog direct-install client choices
// from the backend capability map. Only URL-native (remote-http-capable)
// clients are offered — a relay-stdio client would reject a URL-only entry.
describe("remoteHTTPCapableClients", () => {
  function caps(
    entries: Record<string, boolean>,
  ): Record<string, ClientCapability> {
    const out: Record<string, ClientCapability> = {};
    for (const [c, remote] of Object.entries(entries)) {
      // Every remote-http-capable client is also direct_installable (URL-native);
      // the relay-stdio false-cases are neither. Mirror `remote` into both so the
      // fixture stays realistic (only remote_http_capable is under test here).
      out[c] = { scannable: true, direct_installable: remote, remote_http_capable: remote };
    }
    return out;
  }

  it("returns only the URL-native clients, in canonical column order", () => {
    const got = remoteHTTPCapableClients(
      caps({
        "claude-code": true,
        cursor: true,
        vscode: true,
        // relay-stdio clients: NOT URL-native → excluded.
        aider: false,
        zencoder: false,
        pi: false,
      }),
    );
    expect(got).toEqual(["claude-code", "cursor", "vscode"]);
    // No relay-stdio client leaked in.
    expect(got).not.toContain("aider");
    expect(got).not.toContain("zencoder");
  });

  it("returns an empty list for a null/undefined/empty capability map", () => {
    expect(remoteHTTPCapableClients(null)).toEqual([]);
    expect(remoteHTTPCapableClients(undefined)).toEqual([]);
    expect(remoteHTTPCapableClients({})).toEqual([]);
  });

  it("orders core URL-native clients before non-core ones", () => {
    // A hypothetical non-core URL-native client sorts after the core ones.
    const got = remoteHTTPCapableClients(
      caps({ vscode: true, "claude-code": true, "future-remote": true }),
    );
    // claude-code (core) precedes vscode (core) precedes the unknown extra.
    expect(got[0]).toBe("claude-code");
    expect(got[got.length - 1]).toBe("future-remote");
  });
});

// orderClientsForColumns is the shared ordering authority (used by
// effectiveVisibleClients). CORE first (always, registry order), then
// non-core present-in-input ids in registry order, then unknown extras
// alphabetically.
describe("orderClientsForColumns", () => {
  it("puts the seven core clients first, in registry order, even if absent from input", () => {
    const out = orderClientsForColumns(["zed"]);
    expect(out.slice(0, CORE_CLIENTS.length)).toEqual([...CORE_CLIENTS]);
    expect(out[out.length - 1]).toBe("zed");
  });

  it("orders non-core ids in ALL_CLIENTS (registry) order, not input order", () => {
    const out = orderClientsForColumns(["openclaw", "zed", "cline"]);
    const tail = out.slice(CORE_CLIENTS.length);
    expect(tail).toEqual(
      NON_CORE_CLIENTS.filter((c) => c === "zed" || c === "cline" || c === "openclaw"),
    );
  });

  it("appends unknown (non-registry) ids after known ones, alphabetically", () => {
    const out = orderClientsForColumns(["zzz-client", "aaa-client", "zed"]);
    const tail = out.slice(CORE_CLIENTS.length);
    // zed (known non-core) precedes the two unknown extras; extras sorted.
    expect(tail).toEqual(["zed", "aaa-client", "zzz-client"]);
  });

  it("deduplicates and is stable for the full ALL_CLIENTS input", () => {
    const out = orderClientsForColumns([...ALL_CLIENTS, ...ALL_CLIENTS]);
    expect(out).toEqual([...ALL_CLIENTS]);
  });
});
