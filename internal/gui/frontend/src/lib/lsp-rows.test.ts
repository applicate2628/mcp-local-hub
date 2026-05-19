import { describe, expect, it } from "vitest";
import { collectLspRows, LSP_LANGUAGES } from "./lsp-rows";
import type { ScanResult } from "../types";
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
          client_presence: { "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9201" } },
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
    // ALPHA scope means BETA's entry must NOT bleed in.
    expect(rustAlpha.clientPresence["codex-cli"]?.endpoint).toBe("http://127.0.0.1:9201");
    expect(Object.keys(rustAlpha.legacyConflict)).toEqual([]);

    const rowsForBeta = collectLspRows(scan, wsEntries, "beta");
    const rustBeta = rowsForBeta.find((r) => r.language === "rust")!;
    expect(rustBeta.taskName).toBe("\\mcp-local-hub-lsp-beta-rust-b2cd");
    expect(rustBeta.clientPresence["codex-cli"]?.endpoint).toBe("http://127.0.0.1:9202");
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
});
