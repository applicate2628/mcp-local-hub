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

// TestClientCapabilitiesDirectInstallableMatchesRelayStdio pins the
// direct_installable flag to its SINGLE owner: a client is direct_installable
// iff its adapter is NOT relay-stdio (clients.IsRelayStdio(name) == false) —
// the exact predicate the Catalog direct-install flow needs, since
// writeDirectEntry calls AddEntry({Name, URL}) which a URL-native adapter
// accepts and a relay-stdio adapter rejects.
func TestClientCapabilitiesDirectInstallableMatchesRelayStdio(t *testing.T) {
	caps := ClientCapabilities()

	for name, cap := range caps {
		want := !clients.IsRelayStdio(name)
		if cap.DirectInstallable != want {
			t.Errorf("client %q: DirectInstallable = %v, want %v (== !IsRelayStdio)", name, cap.DirectInstallable, want)
		}
	}

	// The relay-stdio clients MUST be direct_installable=false (a URL-only
	// direct install would be rejected at AddEntry). This is the known
	// relay-stdio set today; if an adapter's IsRelayStdio() flips, update here.
	for _, name := range []string{"aider", "antigravity", "pi", "pochi", "zed", "zencoder"} {
		cap, ok := caps[name]
		if !ok {
			t.Errorf("expected relay-stdio client %q in capability map", name)
			continue
		}
		if cap.DirectInstallable {
			t.Errorf("relay-stdio client %q is direct_installable=true — a URL-only direct install would deterministically fail at AddEntry", name)
		}
	}

	// The URL-native non-core clients hermes/openclaw/opencode MUST be
	// direct_installable=true even though they are OFF the narrow remote-http
	// header matrix (remoteHTTPCapableClients). This is the regression the
	// bot flagged: keying direct-install on remote_http_capable wrongly hid
	// these URL-native adapters. A URL-native adapter's AddEntry accepts a
	// URL-only entry (writeDirectEntry succeeds), so it MUST be offered.
	for _, name := range []string{"hermes", "openclaw", "opencode"} {
		cap, ok := caps[name]
		if !ok {
			t.Errorf("expected URL-native client %q in capability map", name)
			continue
		}
		if !cap.DirectInstallable {
			t.Errorf("URL-native client %q is direct_installable=false — it would be wrongly hidden from Catalog direct-install", name)
		}
		// And it is NOT on the narrow remote-http header matrix — proving the
		// two capabilities are distinct (direct_installable is broader).
		if cap.RemoteHTTPCapable {
			t.Errorf("client %q is on the remote-http header matrix unexpectedly — this test assumes it is URL-native-but-off-matrix", name)
		}
	}

	// direct_installable must be a STRICT superset of remote_http_capable: every
	// remote-http-capable client serializes a URL binding, so it is also
	// URL-native (direct-installable). A regression that made them equal would
	// re-hide the URL-native non-core clients.
	directCount, remoteCount := 0, 0
	for _, cap := range caps {
		if cap.DirectInstallable {
			directCount++
		}
		if cap.RemoteHTTPCapable {
			remoteCount++
			// remote-http-capable ⇒ direct-installable (superset invariant).
		}
	}
	for name, cap := range caps {
		if cap.RemoteHTTPCapable && !cap.DirectInstallable {
			t.Errorf("client %q is remote_http_capable but NOT direct_installable — direct_installable must be a superset", name)
		}
	}
	if directCount <= remoteCount {
		t.Errorf("direct_installable count (%d) must exceed remote_http_capable count (%d) — the URL-native non-core clients are the difference", directCount, remoteCount)
	}
}

// TestClientCapabilitiesRemoteHTTPMatchesMatrix pins the remote_http_capable
// flag to the SINGLE owner remoteHTTPCapableClients (via
// isRemoteHTTPCapableClient). This is the NARROW remote-http manifest/header
// matrix used by the remote-http install plan + draft surfaces — NOT the
// Catalog direct-install client choices (those use direct_installable, pinned
// in TestClientCapabilitiesDirectInstallableMatchesRelayStdio above).
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

func TestClientCapabilitiesAdoptSupportedMatchesAdoptRegistry(t *testing.T) {
	caps := ClientCapabilities()
	adoptSupported := map[string]bool{}
	for _, name := range AdoptSupportedClients() {
		adoptSupported[name] = true
	}

	for name, cap := range caps {
		want := adoptSupported[name]
		if cap.AdoptSupported != want {
			t.Errorf("client %q: AdoptSupported = %v, want %v (matches AdoptSupportedClients)", name, cap.AdoptSupported, want)
		}
	}

	for _, name := range []string{"zed", "kiro", "windsurf", "cline"} {
		cap, ok := caps[name]
		if !ok {
			t.Errorf("expected unsupported adopt-discovery client %q in capability map", name)
			continue
		}
		if cap.AdoptSupported {
			t.Errorf("client %q is adopt_supported=true but /api/adopt/plan rejects it", name)
		}
	}
}
