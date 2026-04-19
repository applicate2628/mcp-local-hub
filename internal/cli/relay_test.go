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
// failed with "open R:\Temp\servers\serena\manifest.yaml: no such
// file or directory" when the binary was invoked from %TEMP%, because
// the old implementation looked for manifests on disk relative to the
// executable path.
func TestResolveRelayURL_ResolvesFromEmbeddedManifest(t *testing.T) {
	// serena ships in the embed FS with daemon "claude" → port 9121.
	u, err := resolveRelayURL("serena", "claude", "")
	if err != nil {
		t.Fatalf("resolveRelayURL(serena, claude): %v", err)
	}
	if !strings.Contains(u, ":9121/mcp") {
		t.Errorf("url = %q, want ...:9121/mcp (serena.claude port)", u)
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
