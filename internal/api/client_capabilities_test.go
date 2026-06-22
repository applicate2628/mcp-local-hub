package api

import (
	"sort"
	"testing"

	"mcp-local-hub/internal/clients"
)

// TestClientCapabilitiesKeyedByEverySupportedClient pins that the capability
// map has exactly one entry per clients.SupportedClientNames() id — no missing
// client (which would hide it from the GUI's capability-derived universe) and
// no extra (a stale id the GUI would surface).
func TestClientCapabilitiesKeyedByEverySupportedClient(t *testing.T) {
	caps := ClientCapabilities()
	registry := clients.SupportedClientNames()
	if len(caps) != len(registry) {
		t.Fatalf("ClientCapabilities() has %d entries, want %d (one per SupportedClientNames())", len(caps), len(registry))
	}
	for _, name := range registry {
		if _, ok := caps[name]; !ok {
			t.Errorf("ClientCapabilities() missing client %q", name)
		}
	}
}

// TestClientCapabilitiesScannableMatchesScannerRegistry pins the scannable
// flag to the SINGLE owner: a client's Scannable is true iff it has a
// clientScanners() parser. This is the drift guard that keeps the GUI's
// "which clients earn a Servers column" decision in lockstep with actual
// scan coverage — a presence-probed-but-unparsed client (copilot-cli,
// amazon-q, openhands, aider) must report scannable=false.
func TestClientCapabilitiesScannableMatchesScannerRegistry(t *testing.T) {
	caps := ClientCapabilities()
	scanners := scannableClientNames()

	for name, cap := range caps {
		want := scanners[name]
		if cap.Scannable != want {
			t.Errorf("client %q: Scannable = %v, want %v (scannable iff it has a clientScanners() parser)", name, cap.Scannable, want)
		}
	}

	// The presence-probed-but-unparsed clients MUST be scannable=false so the
	// GUI gives them no enabled non-core column (Finding 3). This list is the
	// set of SupportedClientNames() ids with no clientScanners() entry today;
	// if a parser is added for one, register it AND drop it here.
	for _, name := range []string{"copilot-cli", "amazon-q", "openhands", "aider"} {
		cap, ok := caps[name]
		if !ok {
			t.Errorf("expected %q in capability map", name)
			continue
		}
		if cap.Scannable {
			t.Errorf("client %q is scannable=true but has no clientScanners() parser — it would get a broken, never-reconcilable Servers cell", name)
		}
	}
}

// TestClientCapabilitiesRemoteHTTPMatchesMatrix pins the remote_http_capable
// flag to the SINGLE owner remoteHTTPCapableClients (via
// isRemoteHTTPCapableClient). The Catalog direct-install flow derives its
// client choices from this, so a relay-stdio client must report
// remote_http_capable=false and never be offered for a direct HTTP install.
func TestClientCapabilitiesRemoteHTTPMatchesMatrix(t *testing.T) {
	caps := ClientCapabilities()

	var got []string
	for name, cap := range caps {
		if cap.RemoteHTTPCapable {
			got = append(got, name)
		}
		if cap.RemoteHTTPCapable != isRemoteHTTPCapableClient(name) {
			t.Errorf("client %q: RemoteHTTPCapable = %v, want %v (matrix owner)", name, cap.RemoteHTTPCapable, isRemoteHTTPCapableClient(name))
		}
	}
	sort.Strings(got)

	want := append([]string(nil), remoteHTTPCapableClients...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("remote-http-capable set = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("remote-http-capable set = %v, want %v", got, want)
		}
	}

	// A relay-stdio client (aider/pi/pochi/zencoder) must NOT be URL-native.
	for _, name := range []string{"aider", "pi", "pochi", "zencoder"} {
		if cap, ok := caps[name]; ok && cap.RemoteHTTPCapable {
			t.Errorf("relay-stdio client %q is remote_http_capable=true — a direct install would deterministically fail", name)
		}
	}
}
