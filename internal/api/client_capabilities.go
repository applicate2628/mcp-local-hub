// internal/api/client_capabilities.go — single source of truth for the
// per-client CAPABILITY flags the GUI needs to decide which client columns
// and direct-install choices it may safely offer.
//
// Four capabilities are surfaced, each derived from the one backend owner of
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
//   - directInstallable  — the client adapter's AddEntry accepts a URL-only
//                          MCPEntry (it is NOT a relay-stdio adapter). This is
//                          the exact predicate the Catalog DIRECT-install flow
//                          needs: writeDirectEntry calls AddEntry({Name, URL})
//                          on the chosen client, which a relay-stdio adapter
//                          (aider, antigravity, pi, pochi, zed, zencoder)
//                          rejects but every URL-native adapter — including the
//                          non-core hermes/openclaw/opencode — accepts. The
//                          owner is clients.IsRelayStdio (each adapter's own
//                          IsRelayStdio() declaration); directInstallable is
//                          its negation.
//   - remoteHTTPCapable  — the client adapter is on the NARROW remote-http
//                          MANIFEST/HEADER matrix (the 6 legacy clients in
//                          remoteHTTPCapableClients: it serializes+round-trips
//                          a transport=remote-http binding WITH Headers). This
//                          is a stricter set than directInstallable and is used
//                          by the remote-http install PLAN gate (install.go) +
//                          the marketplace remote-http DRAFT emission, NOT by
//                          the Catalog direct-install client choices. The owner
//                          is remoteHTTPCapableClients (remote_http_matrix.go).
//   - adoptSupported     — the client id is accepted by mcphub adopt's plan
//                          and execute paths. The owner is AdoptSupportedClients
//                          (adopt.go), so Discovery can hide impossible Adopt
//                          actions for scanned-but-unsupported clients.
//
// These owners already exist and are independently tested; this file only
// PROJECTS them onto a per-client struct the GUI consumes, keyed by every
// clients.SupportedClientNames() id, so the GUI derives its column / direct-
// install / adopt-action universe from the backend instead of a hard-coded
// mirror.

package api

import "mcp-local-hub/internal/clients"

// ClientCapability carries the GUI-relevant capability flags for one client.
type ClientCapability struct {
	// Scannable is true when this client has a clientScanners() parser, so
	// /api/scan reports its per-entry presence truthfully (a migrate into it
	// can later be reconciled / demigrated from the matrix).
	Scannable bool `json:"scannable"`
	// DirectInstallable is true when this client adapter's AddEntry accepts a
	// URL-only MCPEntry — i.e. it is NOT a relay-stdio adapter. This is the
	// exact predicate the Catalog DIRECT-install flow needs (writeDirectEntry
	// calls AddEntry({Name, URL})). Derived from !clients.IsRelayStdio(name) so
	// every URL-native adapter — including the non-core hermes/openclaw/opencode
	// that are off the narrow remote-http header matrix — is offered, while a
	// relay-stdio adapter (which would reject a URL-only entry) is not.
	DirectInstallable bool `json:"direct_installable"`
	// RemoteHTTPCapable is true when this client adapter is on the NARROW
	// remote-http manifest/header matrix (remoteHTTPCapableClients): it
	// serializes + round-trips a transport=remote-http binding WITH Headers.
	// This is a stricter set than DirectInstallable, used by the remote-http
	// install PLAN gate and the marketplace remote-http DRAFT emission — NOT
	// by the Catalog direct-install client choices (those use DirectInstallable).
	RemoteHTTPCapable bool `json:"remote_http_capable"`
	// AdoptSupported is true when /api/adopt/plan accepts this client as a
	// source/target for adopting an unmanaged stdio entry. Derived from
	// AdoptSupportedClients so GUI Discovery never offers an Adopt action that
	// the API will deterministically reject.
	AdoptSupported bool `json:"adopt_supported"`
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
//   - the Servers matrix non-core columns (scannable clients only),
//   - the Catalog direct-install client choices (directInstallable only), and
//   - the remote-http manifest/header surfaces (remoteHTTPCapable only),
//   - the Discovery Adopt action (adoptSupported only),
//
// so none of these surfaces needs a hard-coded client list that could drift
// behind the backend registry.
func ClientCapabilities() map[string]ClientCapability {
	scannable := scannableClientNames()
	adoptSupported := map[string]bool{}
	for _, name := range AdoptSupportedClients() {
		adoptSupported[name] = true
	}
	// relayStdio is built once via clients.RelayStdioClientNames (a single
	// clients.AllClients() pass) instead of calling clients.IsRelayStdio per
	// client name below — IsRelayStdio rebuilds the full adapter set (every
	// factory, including UserHomeDir calls) on each call, so doing that once
	// per SupportedClientNames() entry made this O(N²) in the client count.
	// The result set is identical: each adapter's own IsRelayStdio()
	// declaration remains the single source of truth.
	relayStdio := clients.RelayStdioClientNames()
	out := make(map[string]ClientCapability, len(clients.SupportedClientNames()))
	for _, name := range clients.SupportedClientNames() {
		out[name] = ClientCapability{
			Scannable: scannable[name],
			// directInstallable = AddEntry accepts a URL-only entry = NOT
			// relay-stdio. Derived from the single relayStdio set built
			// above (same per-adapter IsRelayStdio() declaration as
			// clients.IsRelayStdio), so a future URL-native adapter is
			// offered with no edit here.
			DirectInstallable: !relayStdio[name],
			RemoteHTTPCapable: isRemoteHTTPCapableClient(name),
			AdoptSupported:    adoptSupported[name],
		}
	}
	return out
}
