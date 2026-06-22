// internal/api/client_capabilities.go — single source of truth for the
// per-client CAPABILITY flags the GUI needs to decide which client columns
// and direct-install choices it may safely offer.
//
// Two capabilities are surfaced, each derived from the one backend owner of
// that fact so the GUI cannot drift:
//
//   - scannable          — the client has a clientScanners() parser, so
//                          /api/scan can report its per-entry presence
//                          truthfully. A client that is presence-probed but
//                          has NO parser (copilot-cli, amazon-q, openhands,
//                          aider today) can never have its per-entry cell
//                          reconciled after a migrate, so the Servers matrix
//                          must NOT offer it an enabled column. The owner is
//                          clientScanners() (scan.go).
//   - remoteHTTPCapable  — the client adapter accepts a transport=remote-http
//                          (URL-native) binding. The Catalog DIRECT-install
//                          flow writes a remote URL straight into the chosen
//                          client config; a relay-stdio adapter (aider, pi,
//                          pochi, zencoder, …) rejects a URL-only entry at
//                          AddEntry, so a direct install would deterministically
//                          fail. The owner is remoteHTTPCapableClients
//                          (remote_http_matrix.go).
//
// Both sets already exist and are independently tested; this file only
// PROJECTS them onto a per-client struct the GUI consumes, keyed by every
// clients.SupportedClientNames() id, so the GUI derives its column / direct-
// install universe from the backend instead of a hard-coded mirror.

package api

import "mcp-local-hub/internal/clients"

// ClientCapability carries the GUI-relevant capability flags for one client.
type ClientCapability struct {
	// Scannable is true when this client has a clientScanners() parser, so
	// /api/scan reports its per-entry presence truthfully (a migrate into it
	// can later be reconciled / demigrated from the matrix).
	Scannable bool `json:"scannable"`
	// RemoteHTTPCapable is true when this client adapter accepts a
	// transport=remote-http (URL-native) binding — the prerequisite for the
	// Catalog direct-install flow that writes a remote URL straight into the
	// client config.
	RemoteHTTPCapable bool `json:"remote_http_capable"`
}

// scannableClientNames returns the set of client ids that have a
// clientScanners() parser (the keys of clientScanners()). It is the single
// owner of "which clients can be scanned"; ScanFrom dispatches through the
// same map, so this set can never drift from actual scan coverage.
func scannableClientNames() map[string]bool {
	scanners := clientScanners()
	out := make(map[string]bool, len(scanners))
	for name, sc := range scanners {
		if sc.scan != nil {
			out[name] = true
		}
	}
	return out
}

// ClientCapabilities returns the per-client capability map keyed by every
// clients.SupportedClientNames() id. The GUI consumes this (via the scan
// result and the /api/client-capabilities endpoint) to derive:
//
//   - the Servers matrix non-core columns (scannable clients only), and
//   - the Catalog direct-install client choices (remoteHTTPCapable only),
//
// so neither surface needs a hard-coded client list that could drift behind
// the backend registry.
func ClientCapabilities() map[string]ClientCapability {
	scannable := scannableClientNames()
	out := make(map[string]ClientCapability, len(clients.SupportedClientNames()))
	for _, name := range clients.SupportedClientNames() {
		out[name] = ClientCapability{
			Scannable:         scannable[name],
			RemoteHTTPCapable: isRemoteHTTPCapableClient(name),
		}
	}
	return out
}
