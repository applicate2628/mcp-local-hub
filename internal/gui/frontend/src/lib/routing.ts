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
//
// NOTE: this is a pure loopback-SHAPE test (no port check), mirroring the
// backend clients.IsHubHTTPURL. The PORT-aware via-hub gate is layered on
// TOP of it in perClientRouting via loopbackPortMatchesDaemon — a loopback
// URL alone is NOT sufficient to call a cell "via-hub".
export function isHubLoopback(endpoint: string): boolean {
  if (!endpoint) return false;
  try {
    const u = new URL(endpoint);
    return u.hostname === "127.0.0.1" || u.hostname === "localhost" || u.hostname === "[::1]";
  } catch {
    return false;
  }
}

// mirrors api.IsSerenaRouterURL — loopback host + /serena/mcp path,
// port-agnostic.
export function isSerenaRouterURL(endpoint: string): boolean {
  if (!isHubLoopback(endpoint)) return false;
  try {
    return new URL(endpoint).pathname === "/serena/mcp";
  } catch {
    return false;
  }
}

// loopbackEntryPort parses the TCP port out of a hub-shaped loopback URL.
// Returns the port number only when the endpoint isHubLoopback AND carries
// an explicit numeric port; otherwise null (a loopback URL with no explicit
// port, an unparseable URL, or a non-loopback URL). Mirrors the backend
// api.loopbackEntryPort. A loopback URL without an explicit port cannot be a
// hub daemon binding (it would default to :80), so it never matches.
export function loopbackEntryPort(endpoint: string): number | null {
  if (!isHubLoopback(endpoint)) return null;
  try {
    const u = new URL(endpoint);
    if (u.port === "") return null;
    const p = Number(u.port);
    return Number.isInteger(p) && p > 0 ? p : null;
  } catch {
    return null;
  }
}

// loopbackPortMatchesDaemon reports whether a hub-shaped loopback endpoint
// targets one of THIS server's manifest daemon ports. This is the
// load-bearing PORT-aware gate that keeps the matrix cell in lockstep with
// the backend classifier (api.loopbackPortMatchesDaemon): a loopback-http
// cell is "via-hub" only when its URL port is a member of daemonPorts. An
// empty daemonPorts set (no manifest, or the backend predates the
// daemon_ports field) means no loopback entry can match → the cell falls to
// "direct" (unmanaged), never a deceptive green via-hub cell.
export function loopbackPortMatchesDaemon(
  endpoint: string,
  daemonPorts: number[],
): boolean {
  const port = loopbackEntryPort(endpoint);
  if (port === null) return false;
  return daemonPorts.includes(port);
}

// CORE_CLIENTS are the original seven client columns the Servers matrix
// has always rendered. They are ALWAYS shown (even when not detected on
// the host) so the matrix shape is stable and the per-column Initialize
// affordance keeps working for an uninstalled-but-installable core client.
//
// Must stay in sync with the backend scan surface so a manifested server
// with no per-entry presence for, say, claude-code still emits a cell with
// the right "available" / "not-installed" / "unsupported" classification
// driven by `client_config_presence`.
export const CORE_CLIENTS = [
  "claude-code",
  "codex-cli",
  "cursor",
  "vscode",
  "gemini-cli",
  "qwen-cli",
  "antigravity",
] as const;

// WAVE2_CLIENTS are the eight opt-in adapters added in PR #306. With 15
// total clients an always-visible matrix would be unusably wide, so these
// are DETECTION-GATED: a wave-2 column appears only when that client is
// actually present on the host (its config file or parent directory was
// detected, or it already has a server entry). An uninstalled niche client
// adds no column. See visibleClients() for the gating rule.
export const WAVE2_CLIENTS = [
  "zed",
  "kiro",
  "windsurf",
  "cline",
  "kilocode",
  "opencode",
  "hermes",
  "openclaw",
] as const;

// ALL_CLIENTS is the full superset (core + wave-2) in stable order. Used
// for membership tests and as the source order for visibleClients().
export const ALL_CLIENTS = [...CORE_CLIENTS, ...WAVE2_CLIENTS] as const;

// DETECTED_PRESENCE_STATES are the client_config_presence values that mean
// "this client exists on the host in some inspectable form" — the config
// file is present ("ok"), present-but-unwritable ("error"/"error-symlink"),
// or absent-but-its-parent-directory-exists ("missing-init-possible" /
// "missing-init-blocked-symlink"). The only non-detected state is plain
// "missing" (neither file nor parent dir) or absence from the map entirely.
const DETECTED_PRESENCE_STATES = new Set([
  "ok",
  "error",
  "error-symlink",
  "missing-init-possible",
  "missing-init-blocked-symlink",
]);

// visibleClients returns the ordered list of client columns the matrix
// should render for this scan: the seven CORE_CLIENTS unconditionally,
// plus any WAVE2_CLIENTS that are DETECTED on the host. Detection is true
// when the client's client_config_presence state is anything other than
// plain "missing"/absent, OR when at least one scanned server entry already
// references the client (covers a hand-edited config the presence probe
// somehow missed, and keeps a row's existing binding visible).
//
// This keeps the common case (only a couple of editors installed) to a
// narrow matrix while still surfacing every wave-2 client the operator
// actually uses, with no always-15-columns overflow.
export function visibleClients(scan: ScanResult | null | undefined): string[] {
  const ccp = scan?.client_config_presence ?? {};
  const referenced = new Set<string>();
  for (const e of scan?.entries ?? []) {
    for (const c of Object.keys(e.client_presence ?? {})) referenced.add(c);
  }
  const out: string[] = [...CORE_CLIENTS];
  for (const c of WAVE2_CLIENTS) {
    if (DETECTED_PRESENCE_STATES.has(ccp[c] ?? "") || referenced.has(c)) {
      out.push(c);
    }
  }
  return out;
}

// KNOWN_CLIENTS is retained as the full superset for perClientRouting's
// second-pass fill (it must classify every client that could carry a cell,
// not just the visible ones, so a detected wave-2 client's routing is
// computed even before the column-visibility decision). Column VISIBILITY
// is decided by visibleClients(); routing classification covers all.
const KNOWN_CLIENTS = ALL_CLIENTS;

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
  serverName: string = "",
  // PORT-AWARE via-hub: the server's manifest daemon ports (from
  // ScanEntry.daemon_ports). A loopback-http cell is only "via-hub" when
  // its URL port matches one of these. Empty/absent → no loopback cell can
  // match → it falls to "direct". Mirrors the backend classify() gate so
  // the rendered cell agrees with ScanEntry.status. Defaulted to [] so
  // existing callers that don't supply ports stay type-compatible (those
  // loopback cells then render "direct", matching the backend's
  // no-manifest → external decision rather than a deceptive green cell).
  daemonPorts: number[] = [],
): Record<string, Routing> {
  const routing: Record<string, Routing> = {};
  // First pass: signals from per-entry client_presence (existing entries).
  for (const [client, entry] of Object.entries(clientPresence)) {
    const transport = entry?.transport;
    const endpoint = entry?.endpoint ?? "";
    if (!transport || transport === "absent") {
      routing[client] = "not-installed";
    } else if (transport === "http" && isHubLoopback(endpoint)) {
      if (serverName === "serena" && isSerenaRouterURL(endpoint)) {
        routing[client] = "via-hub";
        continue;
      }
      // Loopback SHAPE is necessary but not sufficient — require the URL
      // port to match one of this server's manifest daemon ports. A
      // stale-port loopback entry (e.g. fetch pointed at serena's 9121
      // when fetch's daemon is 9133) is NOT a hub binding for this server;
      // tag it "direct" so the cell renders unchecked/unmanaged exactly as
      // the backend classifies it "external". (Security review: a deceptive
      // green via-hub cell would invite the operator to overwrite/remove a
      // binding that does not point at this server's daemon.)
      routing[client] = loopbackPortMatchesDaemon(endpoint, daemonPorts)
        ? "via-hub"
        : "direct";
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
    } else if (state === "error") {
      // v0.4.5 PR #208 deep-sec Lane B follow-up: a non-IsNotExist
      // stat failure (permissions, ACL anomaly, I/O error) — or a
      // non-regular non-symlink shape (directory, pipe, device) —
      // was previously collapsed into "not-installed" and rendered
      // with the misleading "config file is not present" tooltip.
      // Surface the error state distinctly so the matrix can render a
      // diagnostic tooltip and the operator can take action.
      routing[client] = "config-error";
    } else if (state === "error-symlink") {
      // 2026-05-19 message-accuracy fix: the config path is a symlink
      // the secure-write pipeline refuses in all modes (post-PR #209).
      // Distinct from "config-error" so the matrix renders a
      // symlink-specific tooltip (replace the symlink / edit the
      // target) instead of the misleading generic stat-error message.
      // The cell stays disabled either way (symlinked configs can't be
      // written through), but the diagnostic is now accurate.
      // work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md.
      routing[client] = "config-error-symlink";
    } else {
      // v0.4.5 init-button: "missing-init-possible" still maps to
      // "not-installed" at the per-cell routing level — the matrix
      // cell stays a disabled checkbox until the operator clicks
      // the per-column Initialize button in the header (which writes
      // the empty stub and triggers a scan refresh, after which the
      // state flips to "ok" and the cells become "available"). This
      // keeps the cell state machine identical for "client absent"
      // vs "client present but unconfigured"; only the header
      // affordance distinguishes them.
      //
      // v0.4.5 PR #208 codex r1 F2: "missing-init-blocked-symlink"
      // also falls through here so the cell is a disabled checkbox;
      // the matrix header further suppresses the Initialize button
      // for this state because the hardened init pipeline would
      // refuse the symlinked parent.
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
    routing: perClientRouting(
      e.client_presence ?? {},
      ccp,
      e.can_migrate === true,
      e.name,
      e.daemon_ports ?? [],
    ),
    manifested: e.manifest_exists === true,
    // Task 3.5: propagate the side-channel legacy_conflict map to its
    // camelCase mirror on ServerRow. Task 4.3 consumes this for dual-
    // badge rendering; preserved as undefined when the source ScanEntry
    // does not carry it (json `omitempty` on the Go side).
    legacyConflict: e.legacy_conflict,
  }));
  return out.sort((a, b) => a.name.localeCompare(b.name));
}
