package api

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

// TestLSPRouterPlan_IntendedStateMatchesAdapterReadback is the guard the
// intendedEntryReadbackProjection doc comment names, against a REAL adapter.
//
// The rule under test: an MCPEntry handed to AddEntry is a write REQUEST, not a
// state. lspRouterMCPEntryForClient deliberately fills BOTH `URL` and
// `RelayURL` with the target URL so each adapter family can pick the shape it
// stores — a URL-native adapter writes `url` and a relay-stdio adapter writes
// `command` + `args`. What a later GetEntry returns is therefore a SUBSET of
// what was submitted, and comparing the raw request against that readback makes
// every SUCCESSFUL write look like an unknown third state.
//
// Both the serena forward reconcile and the LSP router plan's IntendedState
// depend on that subset being exactly what intendedEntryReadbackProjection
// predicts. Nothing proved it: the projection's own comment cites this test by
// name, but the test did not exist, so the prediction was an assumption sitting
// under two mutation paths. It is verified here against the live claude-code
// adapter — the URL-native family, which is the one the LSP plan drives.
//
// SAFETY: every client-config path root is redirected to a temp dir before
// clients.AllClients() is called, and only the claude-code adapter is ever
// mutated. It resolves its config to ~/.claude.json, so the write lands in the
// temp dir.
//
// "only claude-code is mutated" was the whole safety argument here and it was not
// sufficient: AllClients() CONSTRUCTS all 47 adapters, so with only HOME /
// USERPROFILE / LOCALAPPDATA redirected the other 43 still resolved the
// operator's real config paths. Nothing read them in this test, but the argument
// was one line of production change away from being wrong. The full non-home set
// is owned by neutralizeClientConfigPathEnv.
func TestLSPRouterPlan_IntendedStateMatchesAdapterReadback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("LOCALAPPDATA", tmp)
	neutralizeClientConfigPathEnv(t, tmp)
	t.Cleanup(SetDaemonStateRootForTest(tmp))

	configPath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(configPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("seed claude-code config: %v", err)
	}

	adapter := clients.AllClients()["claude-code"]
	if adapter == nil {
		t.Fatal("claude-code adapter is missing from clients.AllClients()")
	}
	if got := adapter.ConfigPath(); got != configPath {
		t.Fatalf("FLEET SAFETY: claude-code resolved its config to %s, not the redirected %s — refusing to mutate a path outside the temp home", got, configPath)
	}
	if adapter.IsRelayStdio() {
		t.Fatal("claude-code must be the URL-native family for this test to mean anything")
	}

	const language = "go"
	entryName := LSPRouterEntryName(language)
	targetURL := LSPRouterURL(7777, language)

	request, err := lspRouterMCPEntryForClient(LSPClientRouterOpts{}, adapter, entryName, targetURL)
	if err != nil {
		t.Fatalf("build the write request: %v", err)
	}
	// The premise the projection exists for: the request carries BOTH shapes.
	if request.URL == "" || request.RelayURL == "" {
		t.Fatalf("the write request is supposed to carry both URL and RelayURL so every adapter family can consume it; got %#v", request)
	}

	if err := adapter.AddEntry(request); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	readback, err := adapter.GetEntry(entryName)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if readback == nil {
		t.Fatal("GetEntry returned no entry after a successful AddEntry")
	}

	predicted := intendedEntryReadbackProjection(adapter, request)
	got := lspSnapshotFromEntry("claude-code", language, entryName, readback)
	want := lspSnapshotFromEntry("claude-code", language, entryName, &predicted)
	if !lspSnapshotStateEqual(got, want) {
		t.Fatalf("intendedEntryReadbackProjection does not predict this adapter's readback.\n  readback : %#v\n  predicted: %#v\nThe projection is the single owner of the write-request-to-readback rule; if a real adapter disagrees with it, every caller comparing an intended post-state against a readback is comparing two different shapes.", got, want)
	}
	// State the concrete fact this pins, so a future reader does not have to
	// re-derive it: the URL-native family keeps `url` and drops the relay
	// fields the request also carried.
	if got.URL != targetURL {
		t.Fatalf("URL-native readback lost its url: %#v", got)
	}
	if got.RelayURL != "" {
		t.Fatalf("URL-native readback carried a relay URL: %#v — the projection assumes this family drops it", got)
	}
}
