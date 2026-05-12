package api

import (
	"errors"
	"testing"
)

// TestOpenStateFileAcceptsHubMcpNames pins the invariant that the
// canonical G4 state-file basenames (written by Phase 2+ under
// DaemonStateDir()) pass the single-path-component allowlist enforced
// by validateStateFileName. The existing check already accepts plain
// 'foo-bar.json'-style names; this test guards against a regression
// where a future tightening (e.g. character class restriction) would
// silently reject the hub-mcp.* set.
//
// G4 §"Bind ordering" lists the canonical state-file names; this test
// is the regression guard for that contract.
func TestOpenStateFileAcceptsHubMcpNames(t *testing.T) {
	for _, name := range []string{
		"hub-mcp.lock",
		"hub-mcp-tokens.json",
		"hub-mcp.endpoint.json",
		"hub-mcp-control.token",
		"hub-mcp.log",
	} {
		if err := validateStateFileName(name); err != nil {
			t.Errorf("validateStateFileName(%q) = %v, want nil", name, err)
		}
	}
}

// TestOpenStateFileRejectsHubMcpTraversal confirms that even names
// containing the hub-mcp prefix are rejected when they include path
// separators or parent-dir traversal segments. Defense-in-depth — the
// hub-mcp file family is hardcoded by phase 2-5, but a future caller
// must not be able to escape the state root by prepending hub-mcp.
func TestOpenStateFileRejectsHubMcpTraversal(t *testing.T) {
	for _, name := range []string{
		"hub-mcp/../escape",
		"hub-mcp.lock/../etc",
		"../hub-mcp.lock",
	} {
		if err := validateStateFileName(name); !errors.Is(err, errStateNameInvalid) {
			t.Errorf("validateStateFileName(%q) = %v, want errStateNameInvalid", name, err)
		}
	}
}
