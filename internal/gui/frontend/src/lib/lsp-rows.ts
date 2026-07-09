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

import { entryPointsAtLegacyLSPPort } from "./routing";
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

// LSP_MANIFEST_SERVER is the bare manifest server name for the
// workspace-scoped LSP family. Its per-(workspace, language) proxies
// (`mcp-language-server-<lang>`) are surfaced + enabled through the
// dedicated "LSP daemons" table (collectLspRows), NOT the top
// single-daemon matrix. The matrix's checkbox model is "one server =
// one hub port = one client URL" — it cannot express (project ×
// language), so the bare `mcp-language-server` row rendered a
// non-functional checkbox (Port "—", State "—") that looked like an
// enablement control but could register nothing. Servers.tsx excludes
// this name from the top matrix so the only LSP enablement surface is
// the LSP-daemons table below. (serena is also workspace-scoped but has
// a single path-router endpoint, so its matrix checkbox DOES work and it
// is NOT excluded — the exclusion is by this specific name, not by kind.)
export const LSP_MANIFEST_SERVER = "mcp-language-server";

export function lspRouterEntryName(language: string): string {
  return `${LSP_MANIFEST_SERVER}-${language}`;
}

// lspFallbackEntryNames mirrors the GENERATED half of the backend's
// api.lspLegacyCandidateEntryNames: besides the registry's explicit
// client_entries value, the backend also treats
// `<router>-<workspaceKey[:4]>` and `<router>-<workspaceKey>` as removable for
// a (language, workspace). Those names are client-independent (the Go loop
// never consults clientName), and an older/heuristic registry row carries no
// client_entries at all, so they are the only handle on such an entry.
//
// Single owner on purpose: BOTH the replaceability proof
// (clientPresenceCanEnableFromLegacy) and the scoped-view aggregation set
// (expectedNames) derive from this, so a scoped view can never disagree with
// the ALL-workspaces view about which scan entries belong to a row.
function lspFallbackEntryNames(language: string, workspaceKey: string): string[] {
  const key = workspaceKey ?? "";
  if (key === "") return [];
  const router = lspRouterEntryName(language);
  const short = key.length > 4 ? key.slice(0, 4) : key;
  return [`${router}-${short}`, `${router}-${key}`];
}

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
  // not registered for the active workspace OR when multiple
  // workspaces register the same language in ALL-workspaces mode
  // (ambiguousOwners non-empty in that case). The matrix UI renders
  // a placeholder / disambiguation hint instead of an Edit-env button
  // when taskName is null.
  taskName: string | null;
  // workspaceKey of the row source. Empty string for placeholder
  // rows AND for ambiguous-owner rows in ALL-workspaces mode (since
  // no single workspace owns the row).
  workspaceKey: string;
  // ambiguousOwners lists every workspace_key that registers this
  // language when the operator has NOT scoped the matrix to one
  // workspace AND multiple matches exist. Empty (or undefined) when
  // the row has a single owner OR is a placeholder. Closes bot review
  // PR #222 P2 (lsp-rows.ts:122): pre-fix, ALL-mode with N workspaces
  // for one language silently picked `filteredWs[0]` and wired Edit-
  // env to that workspace's task_name — Apply could then land in the
  // wrong workspace until the operator manually filtered first.
  ambiguousOwners?: string[];
  // ClientPresence + LegacyConflict observed from /api/scan. Combined
  // across every scan entry whose ParseEntryName matches the row's
  // language AND (if a workspace is selected) belongs to that
  // workspace via its registry's client-entry mapping. Placeholder
  // rows have empty objects so the per-client cell renders as
  // "not-installed" via the existing routing fallback.
  clientPresence: Record<string, ClientPresence>;
  // Source scan-entry name for the first-win clientPresence cell. LSP router
  // toggles need this because the backend per-client disable path mutates only
  // the reserved mcp-language-server-<lang> entry, not suffixed siblings.
  clientPresenceEntryName: Record<string, string>;
  // True when the first-win clientPresence points at a legacy /mcp URL through
  // endpoint or relay_url whose port and source entry name are recognized by the
  // backend rollback/ensure owners via the workspace registry. Without this
  // proof, loopback /mcp cells stay unchecked and disabled instead of hitting
  // deterministic ownership errors. The recognized-name set mirrors
  // api.lspLegacyCandidateEntryNames, which includes the registry's generated
  // fallback names, not just explicit client_entries values.
  clientPresenceCanEnableFromLegacy: Record<string, boolean>;
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
    // Owner picking. The "Edit env" affordance needs an unambiguous
    // task_name — when multiple workspaces register the same language
    // in ALL-mode, that's the ambiguity case: we still aggregate
    // per-client presence across them (so the badges remain honest),
    // but `taskName` is left null and `ambiguousOwners` enumerates the
    // candidates so the UI can prompt the operator to pick one via the
    // WorkspaceSelector. Bot review PR #222 P2 (lsp-rows.ts:122).
    let owner: WorkspaceEntryDTO | undefined;
    let ambiguousOwners: string[] | undefined;
    if (filteredWs.length === 1) {
      owner = filteredWs[0];
    } else if (filteredWs.length > 1) {
      // selectedWorkspaceKey !== "" already narrowed `filteredWs` to a
      // single workspace via the filter above, so multi-match here
      // only happens in ALL-workspaces mode. Sort for deterministic
      // ordering (operator sees the same list every refresh).
      ambiguousOwners = filteredWs
        .map((we) => we.workspace_key)
        .sort((a, b) => a.localeCompare(b));
    }
    // Build the set of scan-entry names this row should aggregate over.
    // The registry's ClientEntries map values name each scan-entry name
    // per client — that's the explicit, registry-blessed mapping. When
    // there is no owner (placeholder row), fall back to bare/suffixed
    // name parsing so any unregistered scan entry that LOOKS like this
    // language still surfaces (e.g. a legacy direct-stdio entry an
    // operator never migrated to the workspace registry).
    const expectedNames = new Set<string>();
    // The shared per-language router entry (mcp-language-server-<lang>) serves
    // every registered workspace of that language via path routing. A migrated
    // workspace's client_entries may still name the old per-workspace suffixed
    // entry, so recognize the router name explicitly; rollback reconstructs
    // pre-router entries from client_entries, so the registry is not rewritten.
    const reservedRouterName = lspRouterEntryName(language);
    expectedNames.add(reservedRouterName);
    for (const we of filteredWs) {
      for (const v of Object.values(we.client_entries ?? {})) {
        if (v) expectedNames.add(v);
      }
      // The registry's generated fallback names are part of the backend's
      // removable set (see lspFallbackEntryNames), so they name scan entries
      // this row owns. Without them, a SCOPED view (owner resolved +
      // selectedWorkspaceKey !== "") filtered `mcp-language-server-<lang>-<key4>`
      // out of the aggregation for an older registry row with empty
      // client_entries — the legacy badge and toggle vanished outside
      // ALL-workspaces mode even though the backend recognizes and can replace
      // the entry. Scoped, so a sibling workspace's entries still cannot leak in.
      for (const name of lspFallbackEntryNames(language, we.workspace_key ?? "")) {
        expectedNames.add(name);
      }
    }
    // legacyPorts mirrors api.lspRegistryPortsByLanguage: every registered proxy
    // port for THIS language, across every workspace. Both backend gates — the
    // entryIsHubOwnedLSPClientEntry overwrite check on the reserved target name,
    // and the entryPointsAtLegacyLSPPort check on each legacy candidate name —
    // read that language-wide set, so narrowing to the ports of the single
    // registry row that named the entry refuses cells the backend would replace.
    const legacyPorts = new Set<number>();
    // The legacy candidate names mirror api.lspLegacyCandidateEntryNames: for one
    // (language, client) the backend treats as removable BOTH the registry's
    // explicit client_entries value AND the generated fallbacks (see
    // lspFallbackEntryNames). The generated names are client-independent, so they
    // live in one shared set spanning EVERY workspace of the language — the
    // backend's own candidate set is language-wide, and narrowing it to the
    // selected workspace would refuse cells the backend would replace.
    const generatedLegacyNames = new Set<string>();
    const legacyNamesByClient = new Map<string, Set<string>>();
    for (const we of wsEntries) {
      if (we.language !== language) continue;
      // Candidate-name generation has no port filter in the backend; only the
      // port set (lspRegistryPortsByLanguage) drops port <= 0 rows.
      if (we.port > 0) legacyPorts.add(we.port);
      for (const [client, entryName] of Object.entries(we.client_entries ?? {})) {
        const name = (entryName ?? "").trim();
        if (!name) continue;
        const names = legacyNamesByClient.get(client) ?? new Set<string>();
        names.add(name);
        legacyNamesByClient.set(client, names);
      }
      for (const name of lspFallbackEntryNames(language, we.workspace_key ?? "")) {
        generatedLegacyNames.add(name);
      }
    }
    const isLegacyCandidateName = (client: string, entryName: string): boolean =>
      generatedLegacyNames.has(entryName) ||
      (legacyNamesByClient.get(client)?.has(entryName) ?? false);
    const canEnableFromLegacy = (
      client: string,
      entryName: string,
      presence: ClientPresence,
    ): boolean => {
      // The reserved router name is the backend's own ensure target: it is
      // overwritten whenever entryIsHubOwnedLSPClientEntry holds, and a legacy
      // registry port satisfies that. Any other name must first be a backend
      // removal candidate before its port may prove replaceability.
      if (entryName !== reservedRouterName && !isLegacyCandidateName(client, entryName)) {
        return false;
      }
      return entryPointsAtLegacyLSPPort(presence, legacyPorts);
    };
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
    const clientPresenceEntryName: Record<string, string> = {};
    const clientPresenceCanEnableFromLegacy: Record<string, boolean> = {};
    const legacyConflict: Record<string, ClientEntry> = {};
    // Deterministic, router-preferring aggregation order. The "first match
    // wins" rule below is sensitive to iteration order, so raw scan.entries
    // order must not decide which entry captures a client's presence cell.
    // Prefer the reserved per-language router entry
    // (mcp-language-server-<language>) first, then any remaining siblings by
    // name — so a suffixed legacy entry (mcp-language-server-<language>-<hex>)
    // always lands in legacyConflict, never displaces the router entry.
    // Scope is minimal: this sorts ONLY the entries this row already
    // aggregates over (narrowed to this language via expectedNames); it does
    // not reorder collectServers or any unrelated matrix rows.
    const aggregated = entries
      .filter((e) => expectedNames.has(e.name))
      .sort((a, b) => {
        if (a.name === reservedRouterName && b.name !== reservedRouterName) return -1;
        if (b.name === reservedRouterName && a.name !== reservedRouterName) return 1;
        return a.name.localeCompare(b.name);
      });
    for (const e of aggregated) {
      for (const [client, pres] of Object.entries(e.client_presence ?? {})) {
        // First match wins; later entries that target the same client
        // are coexistence anomalies and surface as legacy_conflict.
        if (!(client in clientPresence)) {
          clientPresence[client] = pres;
          clientPresenceEntryName[client] = e.name;
          clientPresenceCanEnableFromLegacy[client] = canEnableFromLegacy(client, e.name, pres);
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
      ambiguousOwners,
      clientPresence,
      clientPresenceEntryName,
      clientPresenceCanEnableFromLegacy,
      legacyConflict,
    };
  });
}
