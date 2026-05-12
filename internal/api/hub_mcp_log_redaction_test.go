// hub_mcp_log_redaction_test.go — Phase 2 Task 2.1 + 2.5 (G4 unified hub
// MCP). Token redaction unit tests for the RedactToken helper, plus the
// multi-surface golden test that asserts zero plain-token bytes across
// every emit surface in the spec's "Logging hygiene" section.
//
// Task 2.1 covers the three RedactToken cases (replacement, short-hex
// pass-through, multiple occurrences). Task 2.5 extends this file with
// the golden test once the LogHubMcpEvent / wrapHubMcpFileError /
// redactArgvForLog / formatInstallStatusForLog helpers exist.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Logging hygiene + golden test" (F-S2 closure).
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 2.1
// + Task 2.5.

package api

import (
	"strings"
	"testing"
)

// TestRedactTokenReplaces64HexLowercase pins the primary contract: a
// 64-lower-hex run anywhere in the input is replaced with `<token>`.
// The exact bytes of the token MUST NOT survive the call.
func TestRedactTokenReplaces64HexLowercase(t *testing.T) {
	tok := strings.Repeat("a", 64)
	in := "url=http://127.0.0.1:9120/clients/claude-code/mcp token=" + tok + " ok"
	got := RedactToken(in)
	if strings.Contains(got, tok) {
		t.Errorf("token leaked: %q", got)
	}
	if !strings.Contains(got, "<token>") {
		t.Errorf("missing placeholder: %q", got)
	}
}

// TestRedactTokenLeavesShortHexAlone confirms the regex does not over-
// match: anything shorter than 64 hex chars (e.g., a 12-char hash) is
// returned verbatim. Defends against accidentally redacting non-token
// identifiers like commit prefixes or short content hashes.
func TestRedactTokenLeavesShortHexAlone(t *testing.T) {
	in := "hash=" + strings.Repeat("b", 12) + " count=42"
	got := RedactToken(in)
	if got != in {
		t.Errorf("short hex must not be redacted: in=%q got=%q", in, got)
	}
}

// TestRedactTokenHandlesMultipleOccurrences verifies the regex matches
// every 64-hex run rather than only the first. A log line might carry
// both a per-client token and an instance_id (both share the 64-hex
// shape); both must be redacted.
func TestRedactTokenHandlesMultipleOccurrences(t *testing.T) {
	tok1 := strings.Repeat("a", 64)
	tok2 := strings.Repeat("b", 64)
	in := "t1=" + tok1 + " t2=" + tok2
	got := RedactToken(in)
	if strings.Count(got, "<token>") != 2 {
		t.Errorf("expected 2 placeholders; got %q", got)
	}
}
