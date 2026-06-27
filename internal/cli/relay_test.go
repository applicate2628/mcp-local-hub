package cli

import (
	"strings"
	"testing"
)

// TestResolveRelayURL_DirectURL accepts an explicit --url and returns
// it verbatim.
func TestResolveRelayURL_DirectURL(t *testing.T) {
	u, err := resolveRelayURL("", "", "http://localhost:9999/mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "http://localhost:9999/mcp" {
		t.Errorf("url = %q, want http://localhost:9999/mcp", u)
	}
}

// TestResolveRelayURL_MutuallyExclusive rejects mixed flag usage so
// misconfigured invocations fail fast with a clear message.
func TestResolveRelayURL_MutuallyExclusive(t *testing.T) {
	_, err := resolveRelayURL("serena", "claude", "http://x/mcp")
	if err == nil {
		t.Fatal("expected error for --url combined with --server/--daemon")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion: %v", err)
	}
}

// TestResolveRelayURL_ResolvesFromEmbeddedManifest verifies that
// --server/--daemon reads the manifest from the binary's embedded FS.
// This is the regression guard for the original report: the relay
// failed with "open R:\Temp\servers\<server>\manifest.yaml: no such
// file or directory" when the binary was invoked from %TEMP%, because
// the old implementation looked for manifests on disk relative to the
// executable path.
//
// Uses memory/default@9123 — a kind: global server with a static daemon —
// because the legacy --server/--daemon relay form only resolves STATIC
// daemons. serena used to be this fixture (unified@9121), but the area-4
// router-native flip removed serena's static daemons; the dynamic-pool serena
// relay uses the --url form instead (see
// TestResolveRelayURL_DynamicPoolSerena_RejectsServerDaemonForm below and
// internal/clients/antigravity.go's two relay shapes).
func TestResolveRelayURL_ResolvesFromEmbeddedManifest(t *testing.T) {
	u, err := resolveRelayURL("memory", "default", "")
	if err != nil {
		t.Fatalf("resolveRelayURL(memory, default): %v", err)
	}
	if !strings.Contains(u, ":9123/mcp") {
		t.Errorf("url = %q, want ...:9123/mcp (memory.default port)", u)
	}
}

// TestResolveRelayURL_DynamicPoolSerena_RejectsServerDaemonForm pins the area-4
// consequence: the router-native serena catalog has NO static daemons, so the
// legacy --server/--daemon relay form cannot resolve a serena daemon by name.
// The dynamic-pool serena relay (Antigravity) uses the --url form pointing at
// the /serena/mcp router instead — set as MCPEntry.RelayURL by the migrate
// client-reconcile (design §5; internal/clients/antigravity.go). This test
// guards that the now-obsolete --server serena --daemon <name> form fails loud
// rather than silently resolving a stale 9121 URL.
func TestResolveRelayURL_DynamicPoolSerena_RejectsServerDaemonForm(t *testing.T) {
	_, err := resolveRelayURL("serena", "unified", "")
	if err == nil {
		t.Fatal("dynamic-pool serena has no static `unified` daemon; the --server/--daemon relay form must error (the dynamic-pool relay uses --url to the /serena/mcp router)")
	}
	if !strings.Contains(err.Error(), "no daemon") {
		t.Errorf("error should name the missing daemon; got %v", err)
	}
}

// TestResolveRelayURL_MissingFlags rejects invocations with neither
// --url nor --server/--daemon set.
func TestResolveRelayURL_MissingFlags(t *testing.T) {
	cases := []struct {
		server, daemon string
	}{
		{"", ""},
		{"serena", ""},
		{"", "claude"},
	}
	for _, c := range cases {
		_, err := resolveRelayURL(c.server, c.daemon, "")
		if err == nil {
			t.Errorf("expected error for server=%q daemon=%q", c.server, c.daemon)
		}
	}
}
