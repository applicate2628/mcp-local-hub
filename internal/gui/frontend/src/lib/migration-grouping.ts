import type { ScanResult, ScanEntry } from "../types";

// MigrationGroups is the 4-bucket shape the Migration screen renders.
// Names mirror the backend classifier (internal/api/scan.go:classify):
//   - viaHub: entries already routed through the hub (HTTP url pointing
//     at localhost). Readonly display; Demigrate roll-back action only.
//   - canMigrate: stdio entries whose server name matches a manifest
//     in servers/. Pre-checked with Migrate-selected batch action.
//   - unknown: stdio entries with no matching manifest. "Create
//     manifest" button (DISABLED until A2 ships) and "Dismiss".
//   - perSession: entries classified as not-shareable by nature
//     (currently: internal/api/scan.go:perSessionServers). Readonly info.
// An entry classified as "not-installed" (no client has it — scan saw
// the name via a manifest but no config references it) is dropped
// entirely — it has nothing to migrate/demigrate/dismiss.
//
// Dismissed entries are provided by a separate `/api/dismissed` GET
// (see Task 3). The grouping helper filters them out of the Unknown
// group ONLY — never from via-hub / can-migrate / per-session, even
// if the same name appears in dismissedUnknown. This keeps dismissal
// scoped to the Migration screen while /api/scan stays shared with
// Servers and other consumers.
export interface MigrationGroups {
  viaHub: ScanEntry[];
  canMigrate: ScanEntry[];
  unknown: ScanEntry[];
  perSession: ScanEntry[];
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
    perSession: [],
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
        if (dismissedUnknown.has(entry.name)) continue;
        groups.unknown.push(entry);
        break;
      case "per-session":
        groups.perSession.push(entry);
        break;
      default:
        // "not-installed" and malformed/missing status: drop. These
        // have nothing actionable in Migration.
        break;
    }
  }
  groups.viaHub.sort(byName);
  groups.canMigrate.sort(byName);
  groups.unknown.sort(byName);
  groups.perSession.sort(byName);
  return groups;
}
