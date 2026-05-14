import type {
  ClientConfigState,
  ClientPresence,
  Routing,
  ScanResult,
  ServerRow,
} from "../types";

// isHubLoopback reports whether an http endpoint URL targets the local hub.
// MUST parse the URL and compare hostname — a substring test like
// endpoint.includes("127.0.0.1") misclassifies URLs that merely contain the
// loopback string as a DNS label or path/query component. Such a
// misclassification would let Apply rewrite a binding based on the wrong
// routing assumption. Unparseable endpoints (stdio:, relative paths, empty)
// fall to not-loopback.
export function isHubLoopback(endpoint: string): boolean {
  if (!endpoint) return false;
  try {
    const u = new URL(endpoint);
    return u.hostname === "127.0.0.1" || u.hostname === "localhost" || u.hostname === "[::1]";
  } catch {
    return false;
  }
}

// KNOWN_CLIENTS enumerates every client column the Servers matrix
// renders. Must stay in sync with Servers.tsx::CLIENTS so a manifested
// server with no per-entry presence for, say, claude-code still emits a
// cell with the right "available" / "not-installed" / "unsupported"
// classification driven by `client_config_presence`.
const KNOWN_CLIENTS = [
  "claude-code",
  "codex-cli",
  "cursor",
  "vscode",
  "gemini-cli",
  "qwen-cli",
  "antigravity",
] as const;

// perClientRouting maps a server's per-entry client_presence onto per-cell
// routing tags. The cell's tag drives the checkbox visual (checked/
// unchecked) AND interactivity (enabled/disabled).
//
//   "via-hub"       → checked, enabled (uncheck + Apply = demigrate).
//   "direct"        → unchecked, enabled (check + Apply = migrate).
//   "available"     → unchecked, enabled — client config file exists,
//                     no entry for this server yet (operator can
//                     migrate). Bug-bash A2 (#13) closure.
//   "not-installed" → unchecked, disabled — client config absent.
//   "unsupported"   → unchecked, disabled — client cannot host this
//                     server via the hub (e.g., per-session servers).
//
// clientConfigPresence supplies the per-client targetability info that
// per-entry client_presence cannot — an empty `mcpServers: {}` in
// .claude.json gives no per-entry signal that claude-code is targetable,
// only the file's existence does.
export function perClientRouting(
  clientPresence: Record<string, ClientPresence>,
  clientConfigPresence: Record<string, ClientConfigState> = {},
  canMigrate: boolean = true,
): Record<string, Routing> {
  const routing: Record<string, Routing> = {};
  // First pass: signals from per-entry client_presence (existing entries).
  for (const [client, entry] of Object.entries(clientPresence)) {
    const transport = entry?.transport;
    const endpoint = entry?.endpoint ?? "";
    if (!transport || transport === "absent") {
      routing[client] = "not-installed";
    } else if (transport === "http" && isHubLoopback(endpoint)) {
      routing[client] = "via-hub";
    } else if (transport === "relay") {
      routing[client] = "via-hub";
    } else {
      routing[client] = "direct";
    }
  }
  // Second pass: fill cells for known clients NOT in client_presence,
  // using client_config_presence as the source of truth. If the client
  // config file exists ("ok") AND the server is migratable, the cell
  // is "available" — operator can migrate this server into that client.
  // Otherwise the cell is "not-installed".
  //
  // Bot r1 P2 fix: gate "available" to migratable rows only. Pre-fix,
  // /api/scan flags non-manifested entries (clangd, time-server,
  // playwright-as-per-session) with can_migrate=false; without the
  // gate, those rows got enabled checkboxes that hit deterministic
  // /api/migrate errors when clicked.
  for (const client of KNOWN_CLIENTS) {
    if (client in routing) continue;
    const state = clientConfigPresence[client];
    if (state === "ok" && canMigrate) {
      routing[client] = "available";
    } else {
      routing[client] = "not-installed";
    }
  }
  return routing;
}

// collectServers adapts api.ScanResult into a sorted list of ServerRow.
// Sorting by name matches the legacy vanilla-JS render order. The
// `manifested` flag passes through from scan entries so the Servers
// screen can split the main matrix (mcphub-managed manifests) from a
// read-only "Other MCP entries" view of legacy client config entries
// without a corresponding manifest (bug-bash A3 #11/#12).
export function collectServers(scan: ScanResult | null | undefined): ServerRow[] {
  const entries = scan?.entries ?? [];
  const ccp = scan?.client_config_presence ?? {};
  const out: ServerRow[] = entries.map((e) => ({
    name: e.name,
    routing: perClientRouting(e.client_presence ?? {}, ccp, e.can_migrate === true),
    manifested: e.manifest_exists === true,
  }));
  return out.sort((a, b) => a.name.localeCompare(b.name));
}
