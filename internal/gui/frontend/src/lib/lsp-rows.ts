// internal/gui/frontend/src/lib/lsp-rows.ts
//
// v0.5.x Servers-matrix revamp Task 4.3 — synthesizes exactly 9 LSP
// rows (one per manifest-declared language) for the Servers screen.
// The matrix always shows the full language set even when no
// workspace is registered, so the operator sees what's available
// rather than a blank matrix that hides the LSP feature exists.
//
// Three pieces of input compose the rows:
//
//   - `scan.entries`        — what each client config knows today
//                             (mcp-language-server-<lang> entries,
//                             possibly with -<4hex> ownership suffix)
//   - `workspaceEntries`    — what the workspace registry has
//                             registered (one row per (key, lang))
//   - `selectedWorkspaceKey` — operator's filter; ALL = show every
//                             workspace's registration in the row's
//                             ownership column
//
// LSP_LANGUAGES is the canonical 9-language list from
// servers/mcp-language-server/manifest.yaml. Keep both lists in sync
// when the manifest declares a new language.

import type { ClientPresence, ScanResult } from "../types";
import type { ClientEntry } from "../types";
import type { WorkspaceEntryDTO } from "../api";

export const LSP_LANGUAGES = [
  "clangd",
  "fortran",
  "go",
  "javascript",
  "python",
  "rust",
  "typescript",
  "vscode-css",
  "vscode-html",
] as const;

export type LspLanguage = (typeof LSP_LANGUAGES)[number];

// LSP_KNOWN_CLIENTS mirrors the per-client-routing CLIENTS list used by
// Servers.tsx — keeping a local constant lets the row helper produce
// placeholder presence maps without importing from the screen module
// (which would create a circular dep risk once Servers.tsx imports
// from this file).
export const LSP_KNOWN_CLIENTS = [
  "claude-code",
  "codex-cli",
  "cursor",
  "vscode",
  "gemini-cli",
  "qwen-cli",
  "antigravity",
] as const;

export interface LspRow {
  language: LspLanguage;
  // taskName is the canonical leading-backslash supervisor task name
  // (e.g. `\mcp-local-hub-lsp-default-clangd`). NULL when the LSP is
  // not registered for the active workspace — the row renders as a
  // placeholder with no Edit-env affordance.
  taskName: string | null;
  // workspaceKey of the row source. Empty string for placeholder
  // rows (not registered to any workspace).
  workspaceKey: string;
  // ClientPresence + LegacyConflict observed from /api/scan. Combined
  // across every scan entry whose ParseEntryName matches the row's
  // language AND (if a workspace is selected) belongs to that
  // workspace via its registry's client-entry mapping. Placeholder
  // rows have empty objects so the per-client cell renders as
  // "not-installed" via the existing routing fallback.
  clientPresence: Record<string, ClientPresence>;
  legacyConflict: Record<string, ClientEntry>;
}

// parseLspEntryName matches the Go helper at internal/api/manifest_lsp_lookup.go:
// returns the language portion of an `mcp-language-server-<lang>(-<suffix>)?`
// entry name, or null if the name does not match the LSP scheme. Uses
// LONGEST-prefix matching so `vscode-css` is not split into (vscode, css).
function parseLspEntryName(entryName: string): { lang: LspLanguage; suffix: string } | null {
  const prefix = "mcp-language-server-";
  if (!entryName.startsWith(prefix)) return null;
  const tail = entryName.slice(prefix.length);
  // Iterate longest-first so vscode-css / vscode-html beat vscode (which
  // is not even in the list — but the safety still holds for any future
  // overlap such as a hypothetical "rust" vs "rust-analyzer-lang").
  const sortedLangs = [...LSP_LANGUAGES].sort((a, b) => b.length - a.length);
  for (const lang of sortedLangs) {
    if (tail === lang) return { lang, suffix: "" };
    if (tail.startsWith(`${lang}-`)) return { lang, suffix: tail.slice(lang.length + 1) };
  }
  return null;
}

// collectLspRows synthesizes exactly 9 LspRow values — one per declared
// LSP language. Each row's `taskName` resolves through the workspace
// registry when a registration matches the filter; otherwise the row is
// a placeholder. ClientPresence + LegacyConflict are aggregated across
// every scan entry that maps to the row's language so the matrix can
// render presence cells (incl. dual-badge coexistence) without the
// screen having to re-derive the mapping.
export function collectLspRows(
  scan: ScanResult | null | undefined,
  workspaceEntries: WorkspaceEntryDTO[] | null | undefined,
  selectedWorkspaceKey: string,
): LspRow[] {
  const entries = scan?.entries ?? [];
  const wsEntries = workspaceEntries ?? [];
  return LSP_LANGUAGES.map((language) => {
    // Find every registry row whose language matches; respect the
    // selectedWorkspaceKey filter unless it's the ALL sentinel ("").
    const filteredWs = wsEntries.filter(
      (we) =>
        we.language === language &&
        (selectedWorkspaceKey === "" || we.workspace_key === selectedWorkspaceKey),
    );
    // Pick the first matching workspace entry as the row's owner. When
    // multiple workspaces share the same language and the operator has
    // NOT filtered to one, we expose the first one's task_name; the
    // dual-badge rendering still surfaces coexistence per-client.
    const owner = filteredWs[0];
    // Build the set of scan-entry names this row should aggregate over.
    // The registry's ClientEntries map values name each scan-entry name
    // per client — that's the explicit, registry-blessed mapping. When
    // there is no owner (placeholder row), fall back to bare/suffixed
    // name parsing so any unregistered scan entry that LOOKS like this
    // language still surfaces (e.g. a legacy direct-stdio entry an
    // operator never migrated to the workspace registry).
    const expectedNames = new Set<string>();
    for (const we of filteredWs) {
      for (const v of Object.values(we.client_entries ?? {})) {
        if (v) expectedNames.add(v);
      }
    }
    // Cross-reference parsed names — covers placeholders + sanity.
    for (const e of entries) {
      const parsed = parseLspEntryName(e.name);
      if (parsed && parsed.lang === language) {
        // Only fold in the parsed entries when no owner filter is
        // active (placeholder + ALL) OR when the parsed entry name
        // is in the expectedNames set. Otherwise an entry from a
        // different workspace would leak into this row.
        if (
          selectedWorkspaceKey === "" ||
          !owner ||
          expectedNames.has(e.name)
        ) {
          expectedNames.add(e.name);
        }
      }
    }
    const clientPresence: Record<string, ClientPresence> = {};
    const legacyConflict: Record<string, ClientEntry> = {};
    for (const e of entries) {
      if (!expectedNames.has(e.name)) continue;
      for (const [client, pres] of Object.entries(e.client_presence ?? {})) {
        // First match wins; later entries that target the same client
        // are coexistence anomalies and surface as legacy_conflict.
        if (!(client in clientPresence)) {
          clientPresence[client] = pres;
        } else if (!(client in legacyConflict)) {
          legacyConflict[client] = pres;
        }
      }
      for (const [client, le] of Object.entries(e.legacy_conflict ?? {})) {
        if (!(client in legacyConflict)) {
          legacyConflict[client] = le;
        }
      }
    }
    return {
      language,
      taskName: owner?.task_name ?? null,
      workspaceKey: owner?.workspace_key ?? "",
      clientPresence,
      legacyConflict,
    };
  });
}
