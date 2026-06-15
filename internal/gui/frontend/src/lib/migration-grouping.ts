import type { ScanResult, ScanEntry } from "../types";

// Discovery (formerly "Migration") grouping. Buckets mirror the backend
// classifier (internal/api/scan.go:classify):
//   - viaHub: entries already routed through the hub (HTTP url pointing
//     at localhost, or the antigravity/zed relay shape). Read-only display;
//     Demigrate roll-back action only. Carry Managed=true.
//   - canMigrate: stdio entries whose server name matches a manifest
//     in servers/. Pre-checked with Migrate-selected batch action.
//   - unknown: stdio entries with no matching manifest. "Create
//     manifest" button and "Dismiss".
//   - external: client-present NON-hub remote HTTP entries — real external
//     remote MCP servers (e.g. context7 -> mcp.context7.com, qt-docs ->
//     qt.io). They are NOT hub-managed and NOT migrate candidates; they are
//     surfaced read-only so the operator sees ALL MCP servers, not just the
//     hub-managed ones. PRE-FIX these hit the backend "not-installed" branch
//     and were dropped by the default case below — that was the user's
//     "скан не видит все" (scan doesn't see everything) report.
//   - perSession: entries classified as not-shareable by nature
//     (currently: internal/api/scan.go:perSessionServers). Read-only info.
// An entry classified as "not-installed" (NO client references it — scan
// saw the name only via a manifest) is dropped from these actionable
// groups — it has nothing to migrate/demigrate/dismiss/display.
//
// Dismiss rule (documented consistent choice): both `unknown` AND `external`
// are dismiss-filterable. They are the two "unmanaged" buckets the Discovery
// screen groups together under "Unmanaged / External", and an operator who
// dismisses a noisy external remote (or unknown stdio entry) expects it gone
// from the live list and parked in the collapsed "Dismissed" section. The
// hub-owned groups (via-hub / can-migrate / per-session) are NEVER
// dismiss-filtered — dismissal is a Discovery-screen view concern and
// /api/scan stays shared with the Servers matrix and other consumers.
//
// dismissedUnknown is provided by a separate `/api/dismissed` GET; the
// helper ALSO returns the filtered-out entries in `dismissed` so the
// screen can render a collapsed "Dismissed" section instead of losing them.
export interface MigrationGroups {
  viaHub: ScanEntry[];
  canMigrate: ScanEntry[];
  unknown: ScanEntry[];
  external: ScanEntry[];
  perSession: ScanEntry[];
  // Entries hidden from `unknown`/`external` because they appear in
  // dismissedUnknown. Surfaced so the Discovery screen can show a
  // collapsed, expandable "Dismissed" section rather than dropping them.
  dismissed: ScanEntry[];
}

function byName(a: ScanEntry, b: ScanEntry): number {
  return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

export function groupMigrationEntries(
  scan: ScanResult,
  dismissedUnknown: Set<string>,
): MigrationGroups {
  const groups: MigrationGroups = {
    viaHub: [],
    canMigrate: [],
    unknown: [],
    external: [],
    perSession: [],
    dismissed: [],
  };
  const entries = scan.entries ?? [];
  for (const entry of entries) {
    switch (entry.status) {
      case "via-hub":
        groups.viaHub.push(entry);
        break;
      case "can-migrate":
        groups.canMigrate.push(entry);
        break;
      case "unknown":
        if (dismissedUnknown.has(entry.name)) {
          groups.dismissed.push(entry);
          continue;
        }
        groups.unknown.push(entry);
        break;
      case "external":
        // Real external remote MCP (non-hub http). Dismiss-filtered like
        // unknown (see header) so a noisy remote can be parked.
        if (dismissedUnknown.has(entry.name)) {
          groups.dismissed.push(entry);
          continue;
        }
        groups.external.push(entry);
        break;
      case "per-session":
        groups.perSession.push(entry);
        break;
      default:
        // "not-installed" and malformed/missing status: drop. These
        // have nothing actionable in Discovery.
        break;
    }
  }
  groups.viaHub.sort(byName);
  groups.canMigrate.sort(byName);
  groups.unknown.sort(byName);
  groups.external.sort(byName);
  groups.perSession.sort(byName);
  groups.dismissed.sort(byName);
  return groups;
}
