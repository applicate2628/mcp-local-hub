package clients

import "testing"

// relayStdioClientNames is the canonical set of relay-stdio adapters — the
// ones whose AddEntry requires relay context (RelayExePath, and for the
// manifest-lookup form RelayServer/RelayDaemon) and rejects a URL-only
// entry. Exactly antigravity and zed today. A new relay adapter must be
// added here AND declare IsRelayStdio()=true on its concrete struct; the
// SupportedClientNames()-driven loop below fails until both are done.
var relayStdioClientNames = map[string]bool{
	"antigravity": true,
	"zed":         true,
}

// TestIsRelayStdioClassifiesEverySupportedClient asserts the relay-stdio
// predicate is true for exactly {antigravity, zed} and false for every other
// SupportedClientNames() entry — checked through BOTH the per-adapter
// Client.IsRelayStdio method (constructed adapter) and the name-only
// clients.IsRelayStdio helper, so the two stay in lock-step. Driving the
// table off SupportedClientNames() means a future adapter is automatically
// covered: it lands in the loop, and unless it is added to
// relayStdioClientNames AND declares the matching method truth, this test
// fails.
func TestIsRelayStdioClassifiesEverySupportedClient(t *testing.T) {
	all := AllClients()
	for _, name := range SupportedClientNames() {
		want := relayStdioClientNames[name]

		// Package-level name-only helper.
		if got := IsRelayStdio(name); got != want {
			t.Errorf("clients.IsRelayStdio(%q) = %v, want %v", name, got, want)
		}

		// Per-adapter method on the constructed adapter. Every supported
		// client must be constructable on the test host; if not, that is a
		// separate breakage worth surfacing.
		c, ok := all[name]
		if !ok {
			t.Errorf("AllClients() missing adapter for supported client %q", name)
			continue
		}
		if got := c.IsRelayStdio(); got != want {
			t.Errorf("%T.IsRelayStdio() (client %q) = %v, want %v", c, name, got, want)
		}
	}
}

// TestIsRelayStdioUnknownNameIsFalse pins the fail-safe: an unknown client id
// is classified non-relay (false), so a name-only call site never treats a
// typo or stale id as relay-stdio.
func TestIsRelayStdioUnknownNameIsFalse(t *testing.T) {
	if IsRelayStdio("definitely-not-a-real-client") {
		t.Error("clients.IsRelayStdio(unknown) = true, want false (fail-safe non-relay for unknown names)")
	}
}
