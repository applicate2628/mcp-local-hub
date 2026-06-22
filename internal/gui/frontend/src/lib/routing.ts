import type {
  ClientCapability,
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

// NON_CORE_CLIENTS are every client the backend registry knows that is NOT
// one of the seven CORE_CLIENTS. With 46 total clients an always-visible
// matrix would be unusably wide, so these are DETECTION-GATED: a non-core
// column appears only when that client is actually present on the host (its
// config file or parent directory was detected, or it already has a server
// entry). An uninstalled niche client adds no column. See visibleClients()
// for the gating rule.
//
// This list MUST mirror clients.SupportedClientNames() (registry order,
// minus the 7 CORE_CLIENTS) — it is the ordering + membership authority for
// the column toggle menu (MatrixColumnsMenu), the Add/Edit-server binding
// editor (AddServer), the Catalog direct-install multiselect, and the
// Settings → Backups grouping. The DETECTION universe itself, however, is
// derived live from the scan's client_config_presence map (the backend's
// single source of truth, keyed by every SupportedClientNames() client), so
// visibleClients()/perClientRouting() surface a freshly-registered backend
// client the moment it is detected — even before it is added to this list.
// The cross-language drift guard internal/gui/client_registry_drift_test.go
// parses this file and fails `go test ./internal/gui/` if CORE_CLIENTS +
// NON_CORE_CLIENTS falls out of sync with clients.SupportedClientNames().
export const NON_CORE_CLIENTS = [
  // Wave 2 (PR #306): 8 opt-in adapters.
  "zed",
  "kiro",
  "windsurf",
  "cline",
  "kilocode",
  "opencode",
  "hermes",
  "openclaw",
  // agent-skills vendor reconciliation (2026-06-17): 4 file-config agents.
  "copilot-cli",
  "amazon-q",
  "openhands",
  "aider",
  // skills-CLI vendor reconciliation TIER-1 (2026-06-17): 19 file-config agents.
  "bob",
  "codebuddy",
  "command-code",
  "cortex",
  "deepagents",
  "devin",
  "droid",
  "firebender",
  "iflow-cli",
  "junie",
  "kimi-code-cli",
  "kode",
  "ona",
  "pi",
  "qoder",
  "qoder-cn",
  "roo",
  "rovodev",
  "tabnine-cli",
  // skills-CLI vendor reconciliation TIER-2 (2026-06-17): writable global config.
  "warp",
  "continue",
  "goose",
  // skills-CLI vendor reconciliation TIER-2 bespoke (2026-06-17): non-standard key.
  "neovate",
  "crush",
  "pochi",
  "amp",
  "zencoder",
] as const;

// ALL_CLIENTS is the full superset (core + non-core) in stable registry
// order. Used for membership tests and as the source order for
// visibleClients()/effectiveVisibleClients() column ordering.
export const ALL_CLIENTS = [...CORE_CLIENTS, ...NON_CORE_CLIENTS] as const;

// FILE_PRESENT_PRESENCE_STATES are the client_config_presence values that
// mean "an actual config FILE exists on the host for this client" — present
// and readable ("ok"), present-but-unwritable/wrong-shape ("error"), or a
// symlinked config path the secure-write pipeline refuses ("error-symlink").
//
// The detection gate for a DERIVED non-core column is deliberately FILE-
// PRESENT only. The "missing-init-*" states (parent dir exists / is
// creatable but the file itself is absent) are NOT detection for a non-core
// column: on a fresh profile a typical host has dozens of absent-but-
// creatable niche-client config dirs, so counting them as "detected" would
// render every one of the ~39 non-core clients as a column — the overflow
// bug. A non-core client must have a REAL config file before it earns a
// column. (CORE clients stay always-shown regardless — see CORE_CLIENTS —
// so the per-column Initialize affordance for a not-yet-installed core
// client still works on a clean install; G17's clean-install Initialize
// guarantee is a CORE-client guarantee, and the core set is matrix-stable.)
const FILE_PRESENT_PRESENCE_STATES = new Set([
  "ok",
  "error",
  "error-symlink",
]);

// CORE_CLIENT_SET is a fast membership test for the always-shown core set.
const CORE_CLIENT_SET = new Set<string>(CORE_CLIENTS);

// ALL_CLIENT_ORDER maps a known client id → its index in ALL_CLIENTS, the
// canonical registry order. Used to sort the detected non-core columns
// deterministically; a detected client NOT in this map (a backend client
// newer than this frontend list) sorts AFTER all known ones, then
// alphabetically among such extras, so it still surfaces with no edit here.
const ALL_CLIENT_ORDER = new Map<string, number>(
  ALL_CLIENTS.map((c, i) => [c, i]),
);

// nonCoreCandidates derives the set of NON-core client ids the scan knows
// about: every client_config_presence key (the backend probes one per
// clients.SupportedClientNames(), so this is the full backend universe) plus
// every client any scanned entry already binds (covers a hand-edited config
// the presence probe missed, and keeps an existing binding visible), minus
// the always-shown CORE set. Deriving the candidate universe from the scan —
// not a frontend-hardcoded list — is what keeps the matrix from drifting
// behind the backend registry: a newly-registered backend client appears in
// the presence map and therefore here automatically.
function nonCoreCandidates(scan: ScanResult | null | undefined): Set<string> {
  const out = new Set<string>();
  for (const c of Object.keys(scan?.client_config_presence ?? {})) {
    if (!CORE_CLIENT_SET.has(c)) out.add(c);
  }
  for (const e of scan?.entries ?? []) {
    for (const c of Object.keys(e.client_presence ?? {})) {
      if (!CORE_CLIENT_SET.has(c)) out.add(c);
    }
  }
  return out;
}

// orderNonCore sorts a set of non-core client ids into the stable display
// order: known clients in ALL_CLIENTS (registry) order first, then any
// extras (ids not in ALL_CLIENTS) alphabetically. Deterministic regardless
// of Set/map iteration order.
function orderNonCore(ids: Iterable<string>): string[] {
  return [...ids].sort((a, b) => {
    const ia = ALL_CLIENT_ORDER.get(a);
    const ib = ALL_CLIENT_ORDER.get(b);
    if (ia !== undefined && ib !== undefined) return ia - ib;
    if (ia !== undefined) return -1; // known before unknown
    if (ib !== undefined) return 1;
    return a.localeCompare(b); // both unknown → alphabetical
  });
}

// orderClientsForColumns orders an arbitrary set of client ids into the
// canonical column order: the seven CORE_CLIENTS first (always, in registry
// order — even if absent from the input set), then every non-core id present
// in the input in stable order (ALL_CLIENTS registry order, then alphabetical
// extras). This is the ordering authority shared by the Servers matrix
// column logic (effectiveVisibleClients) so a detected client newer than the
// static ALL_CLIENTS list is ordered + shown rather than dropped.
export function orderClientsForColumns(ids: Iterable<string>): string[] {
  const set = new Set(ids);
  const nonCore = orderNonCore([...set].filter((c) => !CORE_CLIENT_SET.has(c)));
  return [...CORE_CLIENTS, ...nonCore];
}

// scannableClients derives the set of client ids the backend can SCAN (it
// has a clientScanners() parser) from the scan's client_capabilities map —
// the backend's single source of truth (api.ClientCapabilities()). A client
// that is presence-probed but has no parser (copilot-cli/amazon-q/openhands/
// aider today) is NOT scannable: /api/scan can never report its per-entry
// presence, so a migrate into it could never be reconciled/demigrated from
// the matrix. Such a client must not earn an enabled non-core column.
//
// When client_capabilities is absent (older backend), this returns an empty
// set, so visibleClients() falls back to the conservative core-only matrix
// rather than guessing — never an overflow, and the drift test pins that the
// frontend's static client universe matches the backend's scannable set.
export function scannableClients(scan: ScanResult | null | undefined): Set<string> {
  const out = new Set<string>();
  for (const [c, cap] of Object.entries(scan?.client_capabilities ?? {})) {
    if (cap?.scannable) out.add(c);
  }
  return out;
}

// orderCapabilityClients sorts a list of capability-derived client ids into the
// canonical multiselect order: core-before-non-core, then registry order, then
// alphabetical extras. UNLIKE orderClientsForColumns it returns ONLY the input
// ids (it never force-includes every core client) — a capability set must offer
// exactly the clients that hold the capability, not the always-shown core set.
// Shared by directInstallableClients + remoteHTTPCapableClients so the two
// capability multiselects order identically.
function orderCapabilityClients(ids: string[]): string[] {
  return ids.sort((a, b) => {
    const ca = CORE_CLIENT_SET.has(a);
    const cb = CORE_CLIENT_SET.has(b);
    if (ca !== cb) return ca ? -1 : 1; // core before non-core
    const ia = ALL_CLIENT_ORDER.get(a);
    const ib = ALL_CLIENT_ORDER.get(b);
    if (ia !== undefined && ib !== undefined) return ia - ib;
    if (ia !== undefined) return -1; // known before unknown
    if (ib !== undefined) return 1;
    return a.localeCompare(b); // both unknown → alphabetical
  });
}

// directInstallableClients derives, from a backend capability map (the scan's
// client_capabilities or the /api/client-capabilities body), the ordered list
// of client ids the Catalog DIRECT-install flow may offer. Direct install
// writes a URL-only MCP entry straight into each chosen client config via the
// adapter's AddEntry; a relay-stdio adapter (aider/antigravity/pi/pochi/zed/
// zencoder) rejects a URL-only entry, so a direct install into it would
// deterministically fail. The backend's `direct_installable` flag is exactly
// !IsRelayStdio (the real "AddEntry accepts URL-only" predicate), so this is
// the correct owner for the direct-install multiselect — BROADER than
// remote_http_capable, which is the narrow 6-client remote-http header matrix.
// Using direct_installable surfaces URL-native non-core adapters (hermes,
// openclaw, opencode) that were wrongly hidden when the multiselect keyed on
// remote_http_capable.
//
// Ordered by the canonical multiselect order. An empty/absent map yields an
// empty list — the caller renders no direct-install choices rather than guessing.
export function directInstallableClients(
  capabilities: Record<string, ClientCapability> | null | undefined,
): string[] {
  const ids: string[] = [];
  for (const [c, cap] of Object.entries(capabilities ?? {})) {
    if (cap?.direct_installable) ids.push(c);
  }
  return orderCapabilityClients(ids);
}

// remoteHTTPCapableClients derives, from a backend capability map, the ordered
// list of client ids on the NARROW remote-http manifest/header matrix (the 6
// legacy clients that serialize + round-trip a transport=remote-http binding
// WITH Headers). This is the owner for the remote-http install plan + draft
// surfaces — NOT the Catalog direct-install client choices, which use the
// broader directInstallableClients (above) instead. Kept distinct so the two
// surfaces can never drift into using each other's (different) client set.
//
// Ordered by the canonical multiselect order. An empty/absent map yields an
// empty list.
export function remoteHTTPCapableClients(
  capabilities: Record<string, ClientCapability> | null | undefined,
): string[] {
  const ids: string[] = [];
  for (const [c, cap] of Object.entries(capabilities ?? {})) {
    if (cap?.remote_http_capable) ids.push(c);
  }
  return orderCapabilityClients(ids);
}

// visibleClients returns the ordered list of client columns the matrix
// should render for this scan: the seven CORE_CLIENTS unconditionally, plus
// any NON-core client that is FULLY SUPPORTED and present on the host.
//
// A derived non-core column appears only when the client is BOTH:
//   (a) SCANNABLE — it has a backend clientScanners() parser (per
//       scan.client_capabilities), so its per-entry presence is truthful and
//       a migrate into it can be reconciled/demigrated; AND
//   (b) FILE-PRESENT — an actual config file exists ("ok"/"error"/
//       "error-symlink"), OR at least one scanned server entry already
//       references it (a real binding keeps its column visible).
//
// The FILE-PRESENT gate (not the broader "parent dir exists / creatable"
// missing-init-* states) is the anti-overflow guarantee: on a fresh profile
// the absent-but-creatable niche-client config dirs do NOT manufacture
// columns. The SCANNABLE gate drops the presence-probed-but-unparsed clients
// that would otherwise get a broken, never-reconcilable cell.
//
// The non-core candidate universe is derived from the scan itself (every
// client_config_presence key the backend probed for, one per
// clients.SupportedClientNames()), NOT a frontend-hardcoded list — so a
// newly-registered backend client surfaces with no edit to this file once it
// is both scannable and detected. This keeps the common case (a couple of
// editors installed) to a narrow matrix while surfacing every fully-supported
// client the operator actually uses, with no overflow.
export function visibleClients(scan: ScanResult | null | undefined): string[] {
  const ccp = scan?.client_config_presence ?? {};
  const scannable = scannableClients(scan);
  const referenced = new Set<string>();
  for (const e of scan?.entries ?? []) {
    for (const c of Object.keys(e.client_presence ?? {})) referenced.add(c);
  }
  const detected: string[] = [];
  for (const c of nonCoreCandidates(scan)) {
    if (!scannable.has(c)) continue; // unparsed client → no truthful cell
    if (FILE_PRESENT_PRESENCE_STATES.has(ccp[c] ?? "") || referenced.has(c)) {
      detected.push(c);
    }
  }
  return [...CORE_CLIENTS, ...orderNonCore(detected)];
}

// routingClassificationClients is the full set of client ids
// perClientRouting's second-pass fill must classify — NOT just the visible
// ones, so a detected client's routing is computed regardless of the
// column-visibility decision. It is the union of the static ALL_CLIENTS
// registry-mirror AND every client_config_presence key the scan supplied
// (the backend's full SupportedClientNames() universe), so a detected
// non-core client — including one newer than ALL_CLIENTS — gets a correctly
// classified cell. Column VISIBILITY is decided by visibleClients(); this
// classification covers everything the scan knows about. Returned in a
// deterministic order (ALL_CLIENTS order, then presence-only extras
// alphabetically) so the cell-fill loop is stable.
function routingClassificationClients(
  clientConfigPresence: Record<string, ClientConfigState>,
): string[] {
  const union = new Set<string>(ALL_CLIENTS);
  for (const c of Object.keys(clientConfigPresence)) union.add(c);
  // Core first (registry order), then non-core in stable order.
  const nonCore = orderNonCore(
    [...union].filter((c) => !CORE_CLIENT_SET.has(c)),
  );
  return [...CORE_CLIENTS, ...nonCore];
}

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
  // The LIVE GUI/hub listener port (ScanResult.gui_port). Used ONLY for serena's
  // /serena/mcp router cell, to apply the SAME live-port check the backend
  // classify uses (IsLiveSerenaRouterURL). 0 (unknown / CLI scan) degrades to the
  // port-agnostic shape check so an early/CLI scan does not falsely flag staleness.
  guiPort: number = 0,
  // The set of SCANNABLE client ids (backend clientScanners() parser, from
  // scan.client_capabilities). Threaded so the second pass can refuse to mark
  // an UNSCANNABLE client's "ok" config as an interactive "available" cell:
  // /api/scan can never report that client's per-entry presence, so after a
  // migrate the cell could never become "via-hub" or be demigrated — an
  // unreconcilable trap. An unscannable client therefore stays disabled
  // ("not-installed") even when its config file exists. When the set is empty/
  // absent (older backend that omits client_capabilities) this gate is INERT —
  // every client is treated as scannable so the legacy behavior (and every test
  // that does not supply capabilities) is preserved; the conservative core-only
  // column gate in visibleClients() already handles the missing-capability case
  // at the column level. CORE clients are always scannable, so the common case
  // is unaffected.
  scannable: Set<string> | null = null,
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
        // Live-port aware, mirroring the backend IsLiveSerenaRouterURL: the
        // router cell is managed ONLY on the live GUI port, so a stale-port entry
        // (a client still pointing at an OLD bound port after the GUI re-bound)
        // renders "direct"/re-migratable — matching the backend's "external" —
        // instead of a deceptive checked cell that hides the unusable URL. guiPort
        // 0 degrades to port-agnostic (Codex #379 r3).
        const port = loopbackEntryPort(endpoint);
        routing[client] = guiPort <= 0 || port === guiPort ? "via-hub" : "direct";
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
      if (serverName === "serena") {
        const relayURL = entry?.relay_url ?? "";
        const port = loopbackEntryPort(relayURL);
        routing[client] =
          isSerenaRouterURL(relayURL) && (guiPort <= 0 || port === guiPort)
            ? "via-hub"
            : "direct";
      } else {
        routing[client] = "via-hub";
      }
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
  // isScannable: when no capability set was supplied (null/empty — older
  // backend), the gate is inert (treat every client as scannable) so legacy
  // behavior + capability-less tests are preserved. Otherwise a client must be
  // a member to earn an interactive "available" cell.
  const isScannable = (client: string): boolean =>
    scannable === null || scannable.size === 0 || scannable.has(client);
  for (const client of routingClassificationClients(clientConfigPresence)) {
    if (client in routing) continue;
    const state = clientConfigPresence[client];
    if (state === "ok" && canMigrate && isScannable(client)) {
      // An UNSCANNABLE client with an "ok" config does NOT reach here — it
      // falls through to "not-installed" below (a disabled cell). Even when an
      // operator force-shows that column via the Clients popover pref, the cell
      // stays non-interactive: routing has no per-entry scan signal for it, so
      // a migrate could never be reconciled/demigrated (Finding 2).
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
  // Derive the scannable set once per scan (not per entry) and thread it into
  // every row's routing so an unscannable client's "ok" config never renders an
  // interactive "available" cell (Finding 2). When client_capabilities is
  // absent the set is empty → the second-pass gate is inert (legacy behavior).
  const scannable = scannableClients(scan);
  const out: ServerRow[] = entries.map((e) => ({
    name: e.name,
    routing: perClientRouting(
      e.client_presence ?? {},
      ccp,
      e.can_migrate === true,
      e.name,
      e.daemon_ports ?? [],
      scan?.gui_port ?? 0,
      scannable,
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
