// parent_path_expand_test.go — Phase 2 Task 2.5 of the v0.5.x Servers
// matrix revamp. Unit tests for the ${parent_path} token expansion
// helper that supervisor uses at spawn time when merging overlay env
// values with the parent process's PATH.
//
// Test contract is documented inline with each test function. Four
// cases cover the documented semantics from
// docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"${parent_path} token semantics" + §"Observability":
//
//  1. Single-substitution happy path: overlay PATH carries the token,
//     parent has PATH set, expansion produces concatenation.
//  2. No-token case: overlay PATH set but lacks ${parent_path}, the
//     `daemon-env-overlay-path-no-parent-token` info event must be
//     emitted to hub-mcp.log (deliberate parent-PATH drop).
//  3. Unknown-token defense: an overlay value with `${unknown}` is
//     rejected with an error rather than being passed through.
//  4. Empty parent PATH: when the parent process has no PATH key,
//     the token expands to an empty string (no error).

package daemon_env_overlay_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

// installStateRoot redirects DaemonStateDir to a t.TempDir so each test
// has an isolated hub-mcp.log it can read assertions against. Uses the
// production-safe SetDaemonStateRootForTest seam (panics outside test
// binaries — see internal/api/testhooks.go:133).
func installStateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	return root
}

// TestExpandParentPath_SingleSubstitution pins the happy path: an
// overlay value carrying ${parent_path} resolves to overlay prefix +
// parent PATH at spawn time. Expansion is single-pass (the token
// payload is not re-scanned).
func TestExpandParentPath_SingleSubstitution(t *testing.T) {
	installStateRoot(t)
	in := map[string]string{"Path": "C:/foo;${parent_path}"}
	parent := []string{"PATH=/sys/bin"}
	got, err := daemon_env_overlay.ExpandParentPath(in, parent)
	if err != nil {
		t.Fatalf("ExpandParentPath: %v", err)
	}
	if got["Path"] != "C:/foo;/sys/bin" {
		t.Errorf("Path = %q, want %q", got["Path"], "C:/foo;/sys/bin")
	}
	// Returned map must be a NEW map — the caller's input must be
	// unchanged (verified by checking the input string is unchanged).
	if in["Path"] != "C:/foo;${parent_path}" {
		t.Errorf("input mutated: in[Path] = %q", in["Path"])
	}
}

// TestExpandParentPath_NoTokenEmitsEvent verifies the deliberate-drop
// pathway: when an overlay's logical PATH key is set but does NOT
// carry ${parent_path}, the operator is deliberately dropping parent
// PATH for that daemon. The supervisor records the decision via a
// `daemon-env-overlay-path-no-parent-token` info event in hub-mcp.log
// so the choice is operator-visible without restarting the GUI.
func TestExpandParentPath_NoTokenEmitsEvent(t *testing.T) {
	dir := installStateRoot(t)
	in := map[string]string{"Path": "C:/foo"}
	parent := []string{"PATH=/sys/bin"}
	got, err := daemon_env_overlay.ExpandParentPath(in, parent)
	if err != nil {
		t.Fatalf("ExpandParentPath: %v", err)
	}
	if got["Path"] != "C:/foo" {
		t.Errorf("Path = %q, want %q (no token must pass through verbatim)", got["Path"], "C:/foo")
	}
	// Read hub-mcp.log per the canonical pattern in
	// hub_mcp_log_redaction_test.go:97-103. The event must be
	// present after the call.
	logPath := filepath.Join(dir, "hub-mcp.log")
	data, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read hub-mcp.log: %v", rerr)
	}
	if !bytes.Contains(data, []byte(`"event":"daemon-env-overlay-path-no-parent-token"`)) {
		t.Errorf("no-parent-token event missing from hub-mcp.log: %s", data)
	}
}

// TestExpandParentPath_UnknownTokenRejected defends against typos and
// unsupported tokens. Only ${parent_path} is supported. Any other
// ${...} placeholder is rejected up-front rather than passed through
// as a literal at spawn time (where it would mislead operators about
// effective PATH).
func TestExpandParentPath_UnknownTokenRejected(t *testing.T) {
	installStateRoot(t)
	in := map[string]string{"Path": "C:/foo;${unknown}"}
	parent := []string{"PATH=/sys/bin"}
	_, err := daemon_env_overlay.ExpandParentPath(in, parent)
	if err == nil {
		t.Fatalf("ExpandParentPath: expected error for unknown token, got nil")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error must name the unknown token: %v", err)
	}
}

// TestExpandParentPath_RejectsPunctuatedToken closes the bot-review
// PR #222 P2 gap: the prior regex `\$\{[A-Za-z0-9_.\-]+\}` allowed
// `${secret:API_KEY}` (the colon is outside the character class) to
// slip past `rejectUnknownTokens`, then `strings.Replace` left it
// as a literal in the env block. The broader regex `\$\{[^}]+\}`
// catches any non-empty placeholder content.
func TestExpandParentPath_RejectsPunctuatedToken(t *testing.T) {
	installStateRoot(t)
	cases := []struct {
		name  string
		value string
	}{
		{"secret-reference", "C:/foo;${secret:API_KEY}"},
		{"path-style-placeholder", "C:/foo;${HOME/.cargo}"},
		{"mixed-with-supported", "C:/foo;${parent_path};${secret:HOME}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := map[string]string{"Path": tc.value}
			parent := []string{"PATH=/sys/bin"}
			_, err := daemon_env_overlay.ExpandParentPath(in, parent)
			if err == nil {
				t.Fatalf("ExpandParentPath: expected error for value %q, got nil", tc.value)
			}
		})
	}
}

// TestExpandParentPath_EmptyParentPath verifies graceful degradation:
// when the parent process has no PATH key, the token expands to an
// empty string and the overlay value becomes the only PATH source.
// Not an error — matches the spec's "spawn proceeds" contract.
func TestExpandParentPath_EmptyParentPath(t *testing.T) {
	installStateRoot(t)
	in := map[string]string{"Path": "C:/foo;${parent_path}"}
	parent := []string{"OTHER=1"} // no PATH key
	got, err := daemon_env_overlay.ExpandParentPath(in, parent)
	if err != nil {
		t.Fatalf("ExpandParentPath: %v", err)
	}
	if got["Path"] != "C:/foo;" {
		t.Errorf("Path = %q, want %q (token must expand to empty string when parent has no PATH)", got["Path"], "C:/foo;")
	}
}
