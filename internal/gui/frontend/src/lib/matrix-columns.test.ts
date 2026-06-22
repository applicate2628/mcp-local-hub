import { describe, expect, it, beforeEach } from "vitest";
import {
  COLUMN_PREFS_KEY,
  loadColumnPrefs,
  saveColumnPrefs,
  clearColumnPrefs,
  effectiveVisibleClients,
  type ColumnPrefs,
} from "./matrix-columns";
import { CORE_CLIENTS, NON_CORE_CLIENTS } from "./routing";
import { installMemoryLocalStorage } from "./test-local-storage";
import type { ScanResult } from "../types";

// happy-dom 20.9.0's globalThis.localStorage is a bare object with no
// Storage methods, so install a Map-backed shim for the persistence tests.
const ls = installMemoryLocalStorage();

// scan builds a ScanResult fixture. visibleClients() now gates non-core
// columns on the SCANNABLE capability (a backend clientScanners() parser), so
// the fixture marks every CORE + NON_CORE client scannable — these tests
// exercise the pref-folding of effectiveVisibleClients, not the scannable
// gate itself (that is covered in routing.test.ts). Any presence key is also
// marked scannable so a 'warp: "ok"' fixture still auto-detects here.
function scan(presence: Record<string, string>): ScanResult {
  const caps: NonNullable<ScanResult["client_capabilities"]> = {};
  for (const c of [
    ...CORE_CLIENTS,
    ...NON_CORE_CLIENTS,
    ...Object.keys(presence),
  ]) {
    caps[c] = { scannable: true, remote_http_capable: false };
  }
  return {
    at: "",
    entries: [],
    client_config_presence: presence as ScanResult["client_config_presence"],
    client_capabilities: caps,
  } as ScanResult;
}

describe("loadColumnPrefs / saveColumnPrefs / clearColumnPrefs", () => {
  beforeEach(() => {
    ls.reset();
  });

  it("returns {} when no prefs are stored", () => {
    expect(loadColumnPrefs()).toEqual({});
  });

  it("round-trips a saved record", () => {
    const prefs: ColumnPrefs = { "claude-code": false, zed: true };
    saveColumnPrefs(prefs);
    expect(loadColumnPrefs()).toEqual(prefs);
    // Stored under the documented key.
    expect(localStorage.getItem(COLUMN_PREFS_KEY)).toBe(
      JSON.stringify(prefs),
    );
  });

  it("returns {} on corrupt JSON", () => {
    localStorage.setItem(COLUMN_PREFS_KEY, "{not json");
    expect(loadColumnPrefs()).toEqual({});
  });

  it("drops non-boolean values and unknown client ids", () => {
    localStorage.setItem(
      COLUMN_PREFS_KEY,
      JSON.stringify({
        "claude-code": true, // valid
        zed: 0, // non-boolean → dropped
        "not-a-real-client": true, // unknown id → dropped
        kiro: false, // valid
      }),
    );
    expect(loadColumnPrefs()).toEqual({ "claude-code": true, kiro: false });
  });

  it("ignores a non-object JSON payload (array / scalar)", () => {
    localStorage.setItem(COLUMN_PREFS_KEY, JSON.stringify(["claude-code"]));
    expect(loadColumnPrefs()).toEqual({});
    localStorage.setItem(COLUMN_PREFS_KEY, JSON.stringify(42));
    expect(loadColumnPrefs()).toEqual({});
  });

  it("clearColumnPrefs removes the stored record", () => {
    saveColumnPrefs({ zed: true });
    clearColumnPrefs();
    expect(localStorage.getItem(COLUMN_PREFS_KEY)).toBeNull();
    expect(loadColumnPrefs()).toEqual({});
  });
});

describe("effectiveVisibleClients", () => {
  it("default (no prefs) equals the auto-detected set", () => {
    // Bare host: only the seven core clients are auto-detected.
    expect(effectiveVisibleClients(scan({}), {})).toEqual([...CORE_CLIENTS]);
  });

  it("default (no prefs) keeps a detected wave-2 client in stable order", () => {
    const cols = effectiveVisibleClients(scan({ zed: "ok" }), {});
    expect(cols).toEqual([...CORE_CLIENTS, "zed"]);
  });

  it("hides a core client the operator explicitly turned off", () => {
    const cols = effectiveVisibleClients(scan({}), { "claude-code": false });
    expect(cols).not.toContain("claude-code");
    // Every other core client survives.
    for (const c of CORE_CLIENTS) {
      if (c !== "claude-code") expect(cols).toContain(c);
    }
  });

  it("shows an undetected wave-2 client the operator explicitly turned on", () => {
    // kiro is NOT auto-detected (absent from presence) but the operator
    // pinned it visible.
    const cols = effectiveVisibleClients(scan({}), { kiro: true });
    expect(cols).toContain("kiro");
    // Appears in ALL_CLIENTS order — after the core set, among wave-2.
    expect(cols).toEqual([...CORE_CLIENTS, "kiro"]);
    // Other undetected wave-2 clients stay hidden.
    expect(cols).not.toContain("zed");
  });

  it("explicit false overrides an auto-detected wave-2 client", () => {
    // zed IS auto-detected, but the operator hid it.
    const cols = effectiveVisibleClients(scan({ zed: "ok" }), { zed: false });
    expect(cols).not.toContain("zed");
    expect(cols).toEqual([...CORE_CLIENTS]);
  });

  it("explicit true is redundant-but-safe for an already-detected client", () => {
    const cols = effectiveVisibleClients(scan({ zed: "ok" }), { zed: true });
    expect(cols).toEqual([...CORE_CLIENTS, "zed"]);
  });

  it("preserves ALL_CLIENTS order when mixing hide-core + show-non-core", () => {
    // Hide one core client, pin two undetected non-core clients (out of
    // declaration order in the prefs object) — the result must still be
    // in ALL_CLIENTS order, not pref-insertion order.
    const prefs: ColumnPrefs = { hermes: true, "codex-cli": false, zed: true };
    const cols = effectiveVisibleClients(scan({}), prefs);
    const expected = [
      ...CORE_CLIENTS.filter((c) => c !== "codex-cli"),
      ...NON_CORE_CLIENTS.filter((c) => c === "zed" || c === "hermes"),
    ];
    expect(cols).toEqual(expected);
  });

  it("shows a NEWER non-core client (warp) when auto-detected", () => {
    // warp is a non-core client added after the original wave-2 set. With
    // the full 46-client universe it must auto-show on detection.
    const cols = effectiveVisibleClients(scan({ warp: "ok" }), {});
    expect(cols).toEqual([...CORE_CLIENTS, "warp"]);
  });

  it("does NOT drop a detected client that is outside the static ALL_CLIENTS list", () => {
    // A backend client newer than the frontend ALL_CLIENTS list, detected via
    // an entry reference, must still survive the ordering loop (which unions
    // ALL_CLIENTS with the auto-detected set) rather than being silently
    // dropped. It sorts to the tail (unknown extra).
    const s: ScanResult = {
      at: "",
      entries: [
        {
          name: "memory",
          client_presence: {
            "future-client-xyz": { transport: "http", endpoint: "http://127.0.0.1:9123/mcp" },
          },
        },
      ],
      client_config_presence: {},
      // A referenced client necessarily came from a scanner (only a scannable
      // client can produce an entry), so it is scannable in the capability map.
      client_capabilities: {
        "future-client-xyz": { scannable: true, remote_http_capable: false },
      },
    } as ScanResult;
    const cols = effectiveVisibleClients(s, {});
    expect(cols).toContain("future-client-xyz");
    expect(cols[cols.length - 1]).toBe("future-client-xyz");
  });

  it("reset semantics: empty prefs returns to the detection default", () => {
    const detected = scan({ windsurf: "ok" });
    // With an override the column set differs...
    const overridden = effectiveVisibleClients(detected, {
      "claude-code": false,
      kiro: true,
    });
    expect(overridden).not.toContain("claude-code");
    expect(overridden).toContain("kiro");
    // ...clearing the prefs (passing {}) snaps back to auto-detection.
    const reset = effectiveVisibleClients(detected, {});
    expect(reset).toEqual([...CORE_CLIENTS, "windsurf"]);
    expect(reset).toContain("claude-code");
    expect(reset).not.toContain("kiro");
  });

  it("tolerates a null/undefined scan (core clients only)", () => {
    expect(effectiveVisibleClients(null, {})).toEqual([...CORE_CLIENTS]);
    expect(effectiveVisibleClients(undefined, { zed: true })).toEqual([
      ...CORE_CLIENTS,
      "zed",
    ]);
  });
});
