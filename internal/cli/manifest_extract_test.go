package cli

import (
	"slices"
	"strings"
	"testing"
)

// TestManifestExtractSupportedClients_IncludesMimocode pins bot PR #420 finding
// 6: `mcphub manifest extract` must offer mimocode (the extract command wires a
// MimoCodeConfigPath, so mimocode IS a supported source), and the required-client
// error + --client flag help render from a SINGLE owner so the two can't drift
// apart again. Before this fix both strings listed only the old 4 clients.
func TestManifestExtractSupportedClients_IncludesMimocode(t *testing.T) {
	if !slices.Contains(manifestExtractSupportedClients, "mimocode") {
		t.Fatalf("mimocode missing from manifestExtractSupportedClients: %v", manifestExtractSupportedClients)
	}
	// The other clients the extract command wires a ScanOpts path for must also
	// be present (claude-code, codex-cli, gemini-cli, antigravity).
	for _, want := range []string{"claude-code", "codex-cli", "gemini-cli", "antigravity"} {
		if !slices.Contains(manifestExtractSupportedClients, want) {
			t.Errorf("extract-supported list missing %q: %v", want, manifestExtractSupportedClients)
		}
	}
	help := manifestExtractClientsHelp()
	if !strings.Contains(help, "mimocode") {
		t.Errorf("extract --client help omits mimocode: %q", help)
	}
	// The single-owner help string must be the source for BOTH the error and the
	// flag help — assert the flag help on the built command renders it.
	cmd := newManifestExtractCmd()
	flag := cmd.Flags().Lookup("client")
	if flag == nil {
		t.Fatal("extract command has no --client flag")
	}
	if !strings.Contains(flag.Usage, "mimocode") {
		t.Errorf("--client flag usage omits mimocode: %q", flag.Usage)
	}
	// The flag usage must render from the same single-owner help (no drift).
	if !strings.Contains(flag.Usage, help) {
		t.Errorf("--client flag usage %q does not embed the single-owner client list %q", flag.Usage, help)
	}
}
