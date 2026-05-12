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
	"bytes"
	"errors"
	"os"
	"path/filepath"
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

// TestRedactionGoldenAcrossAllSurfaces is the multi-surface golden
// test required by spec §"Logging hygiene + golden test" — generate a
// fresh token via the production code path, drive it through every
// emit surface, assert no plain-token bytes survive. Phases 4 and 5
// pipe their emit sites through these helpers; this test would catch
// any new path that bypasses the choke-point.
//
// Surfaces covered:
//
//  1. hub-mcp.log via LogHubMcpEvent.
//  2. Syscall error wrapper via wrapHubMcpFileError.
//  3. Argv echo via redactArgvForLog.
//  4. Install status string via formatInstallStatusForLog.
func TestRedactionGoldenAcrossAllSurfaces(t *testing.T) {
	dir := hubMcpStateTestHelper(t)

	// Generate a fresh token through the production code path.
	tbl, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	tok := tbl.Tokens["claude-code"]
	if len(tok) != 64 {
		t.Fatalf("token shape: %d", len(tok))
	}

	// Surface 1: hub-mcp.log via LogHubMcpEvent. The URL passed in
	// fields carries the token; the marshalled JSON Line must NOT
	// contain plain-token bytes.
	if err := LogHubMcpEvent("info", "test-event", map[string]any{
		"url": "http://127.0.0.1:9120/clients/claude-code/mcp?t=" + tok,
	}); err != nil {
		t.Fatalf("LogHubMcpEvent: %v", err)
	}
	logPath := filepath.Join(dir, "hub-mcp.log")
	logBytes, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read hub-mcp.log: %v", rerr)
	}
	if bytes.Contains(logBytes, []byte(tok)) {
		t.Errorf("token leaked into hub-mcp.log: %s", logBytes)
	}
	if !bytes.Contains(logBytes, []byte("<token>")) {
		t.Errorf("placeholder missing from hub-mcp.log: %s", logBytes)
	}
	// And the log line must still parse as JSON containing the event name.
	if !bytes.Contains(logBytes, []byte(`"event":"test-event"`)) {
		t.Errorf("emit event name lost: %s", logBytes)
	}

	// Surface 2: error wrapper passing token-bearing path. The path
	// arg may carry a token in its basename (e.g.
	// `/tmp/.some-prefix-<token>-tmp`).
	wrapped := wrapHubMcpFileError("write", "/tmp/some-"+tok+"-path", os.ErrPermission)
	if wrapped == nil {
		t.Fatalf("wrapHubMcpFileError returned nil for non-nil cause")
	}
	if strings.Contains(wrapped.Error(), tok) {
		t.Errorf("token leaked into wrapped error: %q", wrapped.Error())
	}
	if !errors.Is(wrapped, os.ErrPermission) {
		t.Errorf("wrapHubMcpFileError dropped %%w wrap; errors.Is(os.ErrPermission) = false")
	}

	// And a nil cause must NOT inflate to an error.
	if wrapHubMcpFileError("write", "/tmp/no-error", nil) != nil {
		t.Errorf("wrapHubMcpFileError(nil-cause) must return nil")
	}

	// Surface 3: argv echo helper. A token mistakenly passed on the
	// command line must NOT survive into the redacted form.
	echoed := redactArgvForLog([]string{"mcphub", "install", "--token", tok})
	joined := strings.Join(echoed, " ")
	if strings.Contains(joined, tok) {
		t.Errorf("token leaked into argv echo: %v", echoed)
	}
	if !strings.Contains(joined, "<token>") {
		t.Errorf("placeholder missing from argv echo: %v", echoed)
	}
	// Non-token elements pass through verbatim.
	if echoed[0] != "mcphub" || echoed[1] != "install" || echoed[2] != "--token" {
		t.Errorf("non-token argv elements mutated: %v", echoed)
	}

	// Surface 4: install status string. The URL passed in carries the
	// token in the query string.
	stat := formatInstallStatusForLog(
		"partial",
		"claude-code",
		"http://127.0.0.1:9120/clients/claude-code/mcp?t="+tok,
	)
	if strings.Contains(stat, tok) {
		t.Errorf("token leaked into install status: %q", stat)
	}
	if !strings.Contains(stat, "<token>") {
		t.Errorf("placeholder missing from install status: %q", stat)
	}
	if !strings.Contains(stat, "claude-code") {
		t.Errorf("client name lost from install status: %q", stat)
	}
}

// TestLogHubMcpEventRotatesAt10MB asserts the rotation contract: when
// the active log is at-or-above 10 MB, the next append rotates to
// `.log.1` before writing the new line. We pre-seed the log file at
// the rotation ceiling with a one-byte sentinel suffix so we can
// distinguish "rotated" (the sentinel is now in .log.1) from "appended"
// (the sentinel is still in the active log).
func TestLogHubMcpEventRotatesAt10MB(t *testing.T) {
	dir := hubMcpStateTestHelper(t)

	logPath := filepath.Join(dir, "hub-mcp.log")
	// Seed 10 MB exactly so the rotation check fires on next call.
	seed := bytes.Repeat([]byte{'x'}, int(HubMcpLogRotateSizeBytes))
	if err := os.WriteFile(logPath, seed, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := LogHubMcpEvent("info", "post-rotation", nil); err != nil {
		t.Fatalf("LogHubMcpEvent: %v", err)
	}

	// .log.1 must now exist and contain the seed.
	rotated, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("read rotated: %v", err)
	}
	if !bytes.Equal(rotated, seed) {
		t.Errorf("rotated content mismatch; len=%d want=%d", len(rotated), len(seed))
	}

	// And the active log holds exactly the new event line.
	active, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if !bytes.Contains(active, []byte(`"event":"post-rotation"`)) {
		t.Errorf("post-rotation event missing from active log: %s", active)
	}
	if bytes.Contains(active, []byte{'x'}) {
		t.Errorf("seed bytes survived rotation into active log: %s", active)
	}
}
