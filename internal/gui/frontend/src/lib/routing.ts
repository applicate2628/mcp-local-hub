import type { ClientPresence, Routing, ScanResult, ServerRow } from "../types";

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

// perClientRouting maps ClientPresence into the per-cell routing tag that
// the Servers matrix expects. Keeps the plan's contract:
//   "via-hub"       → checked cell
//   "not-installed" → disabled cell
//   everything else → unchecked, enabled cell (can be migrated).
export function perClientRouting(
  clientPresence: Record<string, ClientPresence>,
): Record<string, Routing> {
  const routing: Record<string, Routing> = {};
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
  return routing;
}

// collectServers adapts api.ScanResult into a sorted list of ServerRow.
// Sorting by name matches the legacy vanilla-JS render order.
export function collectServers(scan: ScanResult | null | undefined): ServerRow[] {
  const entries = scan?.entries ?? [];
  const out: ServerRow[] = entries.map((e) => ({
    name: e.name,
    routing: perClientRouting(e.client_presence ?? {}),
  }));
  return out.sort((a, b) => a.name.localeCompare(b.name));
}
