import { describe, expect, it } from "vitest";
import { collectLspRows, LSP_LANGUAGES, lspCellState } from "./lsp-rows";
import type { LspCellStateInput, LspOptOutAffordance } from "./lsp-rows";
import type { ClientConfigState, ClientPresence, ScanEntry, ScanResult } from "../types";
import type { WorkspaceEntryDTO } from "../api";

describe("collectLspRows", () => {
  it("returns exactly 9 rows in canonical language order regardless of input emptiness", () => {
    const rows = collectLspRows(null, null, "");
    expect(rows).toHaveLength(9);
    expect(rows.map((r) => r.language)).toEqual([...LSP_LANGUAGES]);
    for (const r of rows) {
      expect(r.taskName).toBeNull();
      expect(r.workspaceKey).toBe("");
      expect(r.clientPresence).toEqual({});
      expect(r.legacyConflict).toEqual({});
    }
  });

  it("populates taskName + workspaceKey from matching workspace entries", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "default",
        workspace_path: "/proj",
        language: "clangd",
        backend: "mcp-language-server",
        port: 9200,
        task_name: "\\mcp-local-hub-lsp-default-clangd",
        client_entries: {},
      },
      {
        workspace_key: "default",
        workspace_path: "/proj",
        language: "rust",
        backend: "mcp-language-server",
        port: 9201,
        task_name: "\\mcp-local-hub-lsp-default-rust",
        client_entries: {},
      },
    ];
    const rows = collectLspRows(null, wsEntries, "");
    const byLang = Object.fromEntries(rows.map((r) => [r.language, r]));
    expect(byLang.clangd.taskName).toBe("\\mcp-local-hub-lsp-default-clangd");
    expect(byLang.clangd.workspaceKey).toBe("default");
    expect(byLang.rust.taskName).toBe("\\mcp-local-hub-lsp-default-rust");
    // Languages without a registry entry stay placeholders.
    expect(byLang.go.taskName).toBeNull();
    expect(byLang.python.taskName).toBeNull();
  });

  it("aggregates client_presence + legacy_conflict from matching scan entries", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "default",
        workspace_path: "/proj",
        language: "clangd",
        backend: "mcp-language-server",
        port: 9200,
        task_name: "\\mcp-local-hub-lsp-default-clangd",
        client_entries: { "codex-cli": "mcp-language-server-clangd" },
      },
    ];
    const scan: ScanResult = {
      at: "2026-05-20T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-clangd",
          manifest_exists: false,
          can_migrate: false,
          status: "via-hub",
          client_presence: {
            "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9200/lsp/clangd" },
          },
          legacy_conflict: {
            "codex-cli": { transport: "stdio", endpoint: "mcp-language-server" },
          },
        },
      ],
    };
    const rows = collectLspRows(scan, wsEntries, "");
    const clangd = rows.find((r) => r.language === "clangd")!;
    expect(clangd.clientPresence["codex-cli"]?.transport).toBe("http");
    expect(clangd.legacyConflict["codex-cli"]?.transport).toBe("stdio");
  });

  it("when a workspace is selected, scopes to that workspace's task_name + client_entries", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "alpha",
        workspace_path: "/proj/alpha",
        language: "rust",
        backend: "mcp-language-server",
        port: 9201,
        task_name: "\\mcp-local-hub-lsp-alpha-rust",
        client_entries: { "codex-cli": "mcp-language-server-rust" },
      },
      {
        workspace_key: "beta",
        workspace_path: "/proj/beta",
        language: "rust",
        backend: "mcp-language-server",
        port: 9202,
        task_name: "\\mcp-local-hub-lsp-beta-rust-b2cd",
        client_entries: { "codex-cli": "mcp-language-server-rust-b2cd" },
      },
    ];
    const scan: ScanResult = {
      at: "2026-05-20T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-rust",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9000/lsp/rust/mcp" } },
        },
        {
          name: "mcp-language-server-rust-b2cd",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9202" } },
        },
      ],
    };

    const rowsForAlpha = collectLspRows(scan, wsEntries, "alpha");
    const rustAlpha = rowsForAlpha.find((r) => r.language === "rust")!;
    expect(rustAlpha.taskName).toBe("\\mcp-local-hub-lsp-alpha-rust");
    expect(rustAlpha.workspaceKey).toBe("alpha");
    // ALPHA scope sees the shared router entry; BETA's suffixed legacy entry must NOT bleed in.
    expect(rustAlpha.clientPresence["codex-cli"]?.endpoint).toBe("http://127.0.0.1:9000/lsp/rust/mcp");
    expect(Object.keys(rustAlpha.legacyConflict)).toEqual([]);

    const rowsForBeta = collectLspRows(scan, wsEntries, "beta");
    const rustBeta = rowsForBeta.find((r) => r.language === "rust")!;
    expect(rustBeta.taskName).toBe("\\mcp-local-hub-lsp-beta-rust-b2cd");
    expect(rustBeta.clientPresence["codex-cli"]?.endpoint).toBe("http://127.0.0.1:9000/lsp/rust/mcp");
  });

  it("recognizes the shared router entry for a migrated workspace whose registry still names a suffixed legacy entry", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "beta",
        workspace_path: "/proj/beta",
        language: "rust",
        backend: "mcp-language-server",
        port: 9202,
        task_name: "\\mcp-local-hub-lsp-beta-rust",
        client_entries: { "codex-cli": "mcp-language-server-rust-b2cd" },
      },
    ];
    const scan: ScanResult = {
      at: "2026-05-20T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-rust",
          manifest_exists: false,
          can_migrate: false,
          status: "via-hub",
          client_presence: { "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9000/lsp/rust/mcp" } },
        },
      ],
    };

    const rows = collectLspRows(scan, wsEntries, "beta");
    const rust = rows.find((r) => r.language === "rust")!;
    expect(rust.taskName).toBe("\\mcp-local-hub-lsp-beta-rust");
    expect(rust.clientPresence["codex-cli"]?.transport).toBe("http");
    expect(rust.clientPresence["codex-cli"]?.endpoint).toBe("http://127.0.0.1:9000/lsp/rust/mcp");
  });

  it("in ALL-mode with multiple workspaces for one language, taskName is null + ambiguousOwners lists all candidates (bot P2.7)", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "beta",
        workspace_path: "/proj/beta",
        language: "rust",
        backend: "mcp-language-server",
        port: 9202,
        task_name: "\\mcp-local-hub-lsp-beta-rust",
        client_entries: { "codex-cli": "mcp-language-server-rust-beta" },
      },
      {
        workspace_key: "alpha",
        workspace_path: "/proj/alpha",
        language: "rust",
        backend: "mcp-language-server",
        port: 9201,
        task_name: "\\mcp-local-hub-lsp-alpha-rust",
        client_entries: { "codex-cli": "mcp-language-server-rust-alpha" },
      },
    ];
    const rows = collectLspRows(null, wsEntries, "");
    const rust = rows.find((r) => r.language === "rust")!;
    // Pre-fix: rust.taskName would silently equal one of the two
    // workspaces' task_name (whichever filteredWs[0] resolved to), and
    // Edit env could send the wrong workspace's task to /api/daemon/env.
    // Fix: taskName=null, ambiguousOwners enumerates the candidates so
    // the UI can prompt the operator to narrow via WorkspaceSelector.
    expect(rust.taskName).toBeNull();
    expect(rust.workspaceKey).toBe("");
    expect(rust.ambiguousOwners).toEqual(["alpha", "beta"]); // sorted
  });

  it("ALL-mode with EXACTLY ONE workspace for a language sets taskName and leaves ambiguousOwners undefined", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "default",
        workspace_path: "/proj",
        language: "rust",
        backend: "mcp-language-server",
        port: 9201,
        task_name: "\\mcp-local-hub-lsp-default-rust",
        client_entries: {},
      },
    ];
    const rows = collectLspRows(null, wsEntries, "");
    const rust = rows.find((r) => r.language === "rust")!;
    expect(rust.taskName).toBe("\\mcp-local-hub-lsp-default-rust");
    expect(rust.workspaceKey).toBe("default");
    expect(rust.ambiguousOwners).toBeUndefined();
  });

  it("parses vscode-css and vscode-html correctly (longest-prefix beats 'vscode')", () => {
    const scan: ScanResult = {
      at: "2026-05-20T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-vscode-css",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { "codex-cli": { transport: "stdio", endpoint: "mcp-language-server" } },
        },
        {
          name: "mcp-language-server-vscode-html-abcd",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { vscode: { transport: "stdio", endpoint: "mcp-language-server" } },
        },
      ],
    };
    const rows = collectLspRows(scan, null, "");
    const css = rows.find((r) => r.language === "vscode-css")!;
    const html = rows.find((r) => r.language === "vscode-html")!;
    expect(css.clientPresence["codex-cli"]?.transport).toBe("stdio");
    expect(html.clientPresence["vscode"]?.transport).toBe("stdio");
  });

  // P3 finding 1: the reserved per-language router entry
  // (mcp-language-server-<language>) must win the "first match" for the
  // presence cell over any suffixed legacy sibling, REGARDLESS of the raw
  // scan.entries order. Pre-fix the aggregation iterated scan.entries in
  // wire order, so a suffixed legacy entry that happened to sort earlier in
  // the scan captured clientPresence and demoted the router entry to
  // legacyConflict — a coexistence-display flip driven purely by ordering.
  it("prefers the reserved router entry over a suffixed legacy sibling in BOTH scan orders (P3 finding 1)", () => {
    const routerEntry: ScanEntry = {
      name: "mcp-language-server-rust",
      manifest_exists: false,
      can_migrate: false,
      status: "via-hub",
      client_presence: {
        "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9000/lsp/rust/mcp" },
      },
    };
    const legacyEntry: ScanEntry = {
      name: "mcp-language-server-rust-b2cd",
      manifest_exists: false,
      can_migrate: false,
      client_presence: {
        "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9202/mcp" },
      },
    };
    // Both wire orders must resolve identically: router → clientPresence,
    // legacy → legacyConflict.
    for (const entries of [
      [routerEntry, legacyEntry],
      [legacyEntry, routerEntry],
    ]) {
      const scan: ScanResult = { at: "2026-05-20T00:00:00Z", entries };
      const rows = collectLspRows(scan, null, "");
      const rust = rows.find((r) => r.language === "rust")!;
      expect(rust.clientPresence["codex-cli"]?.endpoint).toBe(
        "http://127.0.0.1:9000/lsp/rust/mcp",
      );
      expect(rust.legacyConflict["codex-cli"]?.endpoint).toBe(
        "http://127.0.0.1:9202/mcp",
      );
    }
  });

  // Codex PR #524 P2 (lsp-rows.ts:226): the backend's removable-name set
  // (api.lspLegacyCandidateEntryNames) is NOT limited to registry
  // client_entries values — for every workspace key it also generates
  // `mcp-language-server-<lang>-<workspaceKey[:4]>` and
  // `mcp-language-server-<lang>-<workspaceKey>`. An older/heuristic registry row
  // carries no client_entries, so proving replaceability only from
  // client_entries left those legacy cells marked unreplaceable and the GUI
  // disabled a toggle /api/lsp-router/enable would have served.
  it("proves replaceability for backend fallback-named legacy entries with no client_entries (P2)", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "b133f336",
        workspace_path: "D:/dev/project",
        language: "python",
        backend: "mcp-language-server",
        port: 9200,
        task_name: "\\mcp-local-hub-lsp-b133f336-python",
        client_entries: {},
      },
    ];
    const legacyURL = "http://127.0.0.1:9200/mcp";
    const scan: ScanResult = {
      at: "2026-07-09T00:00:00Z",
      entries: [
        {
          // short (4-char) workspace-key fallback name
          name: "mcp-language-server-python-b133",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { "codex-cli": { transport: "http", endpoint: legacyURL } },
        },
        {
          // full workspace-key fallback name
          name: "mcp-language-server-python-b133f336",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { cursor: { transport: "http", endpoint: legacyURL } },
        },
        {
          // NEGATIVE: a suffix the backend never generates for this registry.
          name: "mcp-language-server-python-zzzz",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { "gemini-cli": { transport: "http", endpoint: legacyURL } },
        },
      ],
    };
    const python = collectLspRows(scan, wsEntries, "").find((r) => r.language === "python")!;
    expect(python.clientPresenceEntryName["codex-cli"]).toBe("mcp-language-server-python-b133");
    expect(python.clientPresenceCanEnableFromLegacy["codex-cli"]).toBe(true);
    expect(python.clientPresenceEntryName["cursor"]).toBe("mcp-language-server-python-b133f336");
    expect(python.clientPresenceCanEnableFromLegacy["cursor"]).toBe(true);
    expect(python.clientPresenceEntryName["gemini-cli"]).toBe("mcp-language-server-python-zzzz");
    expect(python.clientPresenceCanEnableFromLegacy["gemini-cli"]).toBe(false);
  });

  // A backend candidate name is necessary but not sufficient: the legacy /mcp
  // URL's port must also be a registered proxy port for the language, else the
  // enable pass would hit a deterministic ownership error.
  it("refuses a fallback-named legacy entry whose port is not in the registry", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "b133f336",
        workspace_path: "D:/dev/project",
        language: "python",
        backend: "mcp-language-server",
        port: 9200,
        task_name: "\\mcp-local-hub-lsp-b133f336-python",
        client_entries: {},
      },
    ];
    const scan: ScanResult = {
      at: "2026-07-09T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-python-b133",
          manifest_exists: false,
          can_migrate: false,
          client_presence: {
            "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9999/mcp" },
          },
        },
      ],
    };
    const python = collectLspRows(scan, wsEntries, "").find((r) => r.language === "python")!;
    expect(python.clientPresenceCanEnableFromLegacy["codex-cli"]).toBe(false);
  });

  // Codex PR #524 round-2 P2 (lsp-rows.ts:197): the generated fallback names
  // were folded into the REPLACEABILITY proof but never into `expectedNames`,
  // which is what the scoped-view aggregation filters on. With a workspace
  // selected (owner set), a fallback-named scan entry from an older registry row
  // with empty client_entries therefore vanished from the row entirely — no
  // presence, no legacy badge, no toggle — while ALL-workspaces mode showed it.
  describe("scoped view agrees with ALL-workspaces view on fallback candidate names", () => {
    const wsEntries: WorkspaceEntryDTO[] = [
      {
        workspace_key: "b133f336",
        workspace_path: "D:/dev/project",
        language: "python",
        backend: "mcp-language-server",
        port: 9200,
        task_name: "\\mcp-local-hub-lsp-b133f336-python",
        // Older/heuristic registry row: the backend derives the removable
        // entry names from the workspace key, not from this map.
        client_entries: {},
      },
    ];
    const legacyURL = "http://127.0.0.1:9200/mcp";
    const scan: ScanResult = {
      at: "2026-07-09T00:00:00Z",
      entries: [
        {
          // short (4-char) workspace-key fallback name
          name: "mcp-language-server-python-b133",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { "codex-cli": { transport: "http", endpoint: legacyURL } },
        },
        {
          // full workspace-key fallback name
          name: "mcp-language-server-python-b133f336",
          manifest_exists: false,
          can_migrate: false,
          client_presence: { cursor: { transport: "http", endpoint: legacyURL } },
        },
      ],
    };

    it("yields the same row, badge and toggle-enabled state in both views", () => {
      const all = collectLspRows(scan, wsEntries, "").find((r) => r.language === "python")!;
      const scoped = collectLspRows(scan, wsEntries, "b133f336").find(
        (r) => r.language === "python",
      )!;
      expect(scoped.taskName).toBe(all.taskName);
      expect(scoped.clientPresenceEntryName).toEqual(all.clientPresenceEntryName);
      expect(scoped.clientPresence).toEqual(all.clientPresence);
      expect(scoped.clientPresenceCanEnableFromLegacy).toEqual(
        all.clientPresenceCanEnableFromLegacy,
      );
      // Both fallback shapes are recognized and replaceable.
      expect(scoped.clientPresenceEntryName["codex-cli"]).toBe("mcp-language-server-python-b133");
      expect(scoped.clientPresenceCanEnableFromLegacy["codex-cli"]).toBe(true);
      expect(scoped.clientPresenceEntryName["cursor"]).toBe(
        "mcp-language-server-python-b133f336",
      );
      expect(scoped.clientPresenceCanEnableFromLegacy["cursor"]).toBe(true);
    });

    // NEGATIVE: a suffix the backend never generates for this registry stays
    // excluded from the scoped view (it belongs to no selected workspace) and
    // stays unreplaceable in the ALL view (where unregistered LSP-shaped entries
    // are deliberately surfaced so the operator can see them, but the toggle
    // must not offer to overwrite them).
    it("keeps an unknown-suffix name excluded when scoped and disabled in ALL mode", () => {
      const withUnknown: ScanResult = {
        at: "2026-07-09T00:00:00Z",
        entries: [
          {
            name: "mcp-language-server-python-zzzz",
            manifest_exists: false,
            can_migrate: false,
            client_presence: { "gemini-cli": { transport: "http", endpoint: legacyURL } },
          },
        ],
      };
      const scoped = collectLspRows(withUnknown, wsEntries, "b133f336").find(
        (r) => r.language === "python",
      )!;
      expect(scoped.clientPresence["gemini-cli"]).toBeUndefined();
      expect(scoped.clientPresenceEntryName["gemini-cli"]).toBeUndefined();
      expect(scoped.clientPresenceCanEnableFromLegacy["gemini-cli"]).toBeUndefined();

      const all = collectLspRows(withUnknown, wsEntries, "").find((r) => r.language === "python")!;
      expect(all.clientPresenceEntryName["gemini-cli"]).toBe("mcp-language-server-python-zzzz");
      expect(all.clientPresenceCanEnableFromLegacy["gemini-cli"]).toBe(false);
    });
  });
});

// lspCellState is the single owner of the LSP cell's three rendered decisions:
// checkbox checked, checkbox enabled, and which affordance (if any) persists
// clients.lsp_router_disabled. The table below is the exhaustive
// entry-presence enumeration; every row asserts all three so a future gate
// change cannot silently strand a shape without an opt-out path.
describe("lspCellState", () => {
  const LANG = "go";
  const RESERVED = "mcp-language-server-go";
  const ROUTER_URL = "http://127.0.0.1:9125/lsp/go/mcp";
  const LEGACY_URL = "http://127.0.0.1:9200/mcp";

  const base = {
    language: LANG,
    presence: undefined as ClientPresence | undefined,
    entryName: undefined as string | undefined,
    canEnableFromLegacy: false,
    configState: "ok" as ClientConfigState | undefined,
    checkedOverride: undefined as boolean | undefined,
    clientRouterDisabled: false,
    busy: false,
  };

  interface Shape {
    name: string;
    input: LspCellStateInput;
    checked: boolean;
    checkboxDisabled: boolean;
    optOutAffordance: LspOptOutAffordance;
  }

  const shapes: Shape[] = [
    {
      name: "no entries",
      input: { ...base },
      checked: false,
      checkboxDisabled: false,
      optOutAffordance: "button",
    },
    {
      name: "router entry",
      input: {
        ...base,
        presence: { transport: "http", endpoint: ROUTER_URL },
        entryName: RESERVED,
      },
      checked: true,
      checkboxDisabled: false,
      optOutAffordance: "checkbox",
    },
    {
      name: "router entry via mcphub relay",
      input: {
        ...base,
        presence: { transport: "relay", endpoint: "C:/tools/mcphub.exe", relay_url: ROUTER_URL },
        entryName: RESERVED,
      },
      checked: true,
      checkboxDisabled: false,
      optOutAffordance: "checkbox",
    },
    {
      name: "legacy HTTP entry, registry-recognized",
      input: {
        ...base,
        presence: { transport: "http", endpoint: LEGACY_URL },
        entryName: RESERVED,
        canEnableFromLegacy: true,
      },
      checked: false,
      checkboxDisabled: false,
      optOutAffordance: "button",
    },
    {
      name: "legacy HTTP entry, orphaned (no registry evidence)",
      input: {
        ...base,
        presence: { transport: "http", endpoint: LEGACY_URL },
        entryName: RESERVED,
        canEnableFromLegacy: false,
      },
      checked: false,
      checkboxDisabled: true,
      optOutAffordance: "button",
    },
    {
      name: "legacy relay entry, registry-recognized",
      input: {
        ...base,
        presence: { transport: "relay", endpoint: "C:/tools/mcphub.exe", relay_url: LEGACY_URL },
        entryName: RESERVED,
        canEnableFromLegacy: true,
      },
      checked: false,
      checkboxDisabled: false,
      optOutAffordance: "button",
    },
    {
      name: "suffixed sibling with router URL",
      input: {
        ...base,
        presence: { transport: "http", endpoint: ROUTER_URL },
        entryName: "mcp-language-server-go-b133",
      },
      checked: false,
      checkboxDisabled: true,
      optOutAffordance: "button",
    },
    {
      name: "inherited router entry",
      input: {
        ...base,
        presence: { transport: "http", endpoint: ROUTER_URL, inherited: true },
        entryName: RESERVED,
      },
      checked: true,
      checkboxDisabled: true,
      optOutAffordance: "button",
    },
    {
      name: "presence.disabled router entry",
      input: {
        ...base,
        presence: { transport: "http", endpoint: ROUTER_URL, disabled: true },
        entryName: RESERVED,
      },
      checked: false,
      checkboxDisabled: false,
      optOutAffordance: "button",
    },
    {
      name: "foreign relay to the router URL",
      input: {
        ...base,
        presence: { transport: "relay", endpoint: "node", relay_url: ROUTER_URL },
        entryName: RESERVED,
      },
      checked: false,
      checkboxDisabled: true,
      optOutAffordance: "button",
    },
    {
      name: "non-loopback remote HTTP entry",
      input: {
        ...base,
        presence: { transport: "http", endpoint: "https://lsp.example.com/lsp/go/mcp" },
        entryName: RESERVED,
      },
      checked: false,
      checkboxDisabled: true,
      optOutAffordance: "button",
    },
    {
      name: "unusable config",
      input: { ...base, configState: "missing" },
      checked: false,
      checkboxDisabled: true,
      optOutAffordance: "unavailable",
    },
    {
      name: "unusable config with a live router entry",
      input: {
        ...base,
        presence: { transport: "http", endpoint: ROUTER_URL },
        entryName: RESERVED,
        configState: "error",
      },
      checked: true,
      checkboxDisabled: false,
      optOutAffordance: "checkbox",
    },
    {
      name: "already opted out",
      input: { ...base, clientRouterDisabled: true },
      checked: false,
      checkboxDisabled: false,
      optOutAffordance: "already-off",
    },
    {
      name: "optimistic opt-out override",
      input: { ...base, checkedOverride: false, clientRouterDisabled: false },
      checked: false,
      checkboxDisabled: false,
      optOutAffordance: "already-off",
    },
    {
      name: "apply in flight",
      input: { ...base, busy: true },
      checked: false,
      checkboxDisabled: true,
      optOutAffordance: "unavailable",
    },
  ];

  for (const shape of shapes) {
    it(`${shape.name}: checked=${shape.checked} disabled=${shape.checkboxDisabled} optOut=${shape.optOutAffordance}`, () => {
      const state = lspCellState(shape.input);
      expect(state.checked).toBe(shape.checked);
      expect(state.checkboxDisabled).toBe(shape.checkboxDisabled);
      expect(state.optOutAffordance).toBe(shape.optOutAffordance);
      expect(state.showOptOutButton).toBe(shape.optOutAffordance === "button");
    });
  }

  // The class invariant the round-5 and round-7 bot findings both violated:
  // an entry-presence shape must never decide whether the opt-out preference
  // is reachable. Only a usable config, a pending Apply, and an already-
  // persisted opt-out may.
  it("always exposes an opt-out path for a usable, idle, not-yet-opted-out client", () => {
    for (const shape of shapes) {
      const input = {
        ...shape.input,
        configState: "ok" as ClientConfigState,
        busy: false,
        checkedOverride: undefined,
        clientRouterDisabled: false,
      };
      const state = lspCellState(input);
      expect(["checkbox", "button"]).toContain(state.optOutAffordance);
    }
  });
});
