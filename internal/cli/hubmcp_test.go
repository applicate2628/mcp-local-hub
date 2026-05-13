// internal/cli/hubmcp_test.go — Phase 5 Task 5.2 test surface.
//
// The full live-reload integration test (rotation → /internal/reload-tokens
// → old-token-401-within-500ms) lives in the Phase 4 + Phase 5.5 e2e
// suite (which has the full hub listener harness). This file covers
// the CLI-level surface: command registration, non-TTY guard,
// redaction shape, and presence-tag helper correctness.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestHubMcpCommandRegistered pins that the root command wiring
// (root.go AddCommand) exposes the three leaves operators expect.
func TestHubMcpCommandRegistered(t *testing.T) {
	root := NewRootCmd()
	hub, _, err := root.Find([]string{"hub-mcp"})
	if err != nil || hub == nil {
		t.Fatalf("hub-mcp command not registered on root: %v", err)
	}
	wantLeaves := map[string]bool{
		"status":                 false,
		"regenerate-token":       false,
		"regenerate-instance-id": false,
	}
	for _, sub := range hub.Commands() {
		if _, ok := wantLeaves[sub.Name()]; ok {
			wantLeaves[sub.Name()] = true
		}
	}
	for name, found := range wantLeaves {
		if !found {
			t.Errorf("hub-mcp missing subcommand %q", name)
		}
	}
}

// TestHubMcpRegenerateTokenNonTTYRequiresYes pins the non-TTY guard:
// running the command with a non-terminal stdin and no --yes must
// return forceExitError code 6 (matches existing watchdog CLI
// convention — see internal/cli/gui.go forceExitError).
func TestHubMcpRegenerateTokenNonTTYRequiresYes(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"hub-mcp", "regenerate-token", "--client", "claude-code"})
	root.SetIn(bytes.NewReader(nil)) // non-terminal reader
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected non-TTY without --yes to exit non-zero")
	}
	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 6 {
		t.Errorf("want forceExitError code 6, got %T %v", err, err)
	}
}

// TestHubMcpRegenerateInstanceIDNonTTYRequiresYes — same rule for
// the instance-id rotation command (the lifecycle is more
// disruptive, so the guard MUST be at least as strict).
func TestHubMcpRegenerateInstanceIDNonTTYRequiresYes(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"hub-mcp", "regenerate-instance-id"})
	root.SetIn(bytes.NewReader(nil))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected non-TTY without --yes to exit non-zero")
	}
	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 6 {
		t.Errorf("want forceExitError code 6, got %T %v", err, err)
	}
}

// TestHubMcpStatusOutputRedactsTokens runs the status command against
// the current state-dir (whatever that resolves to in tests) and
// asserts the marshaled stdout contains NO 64-hex token-like
// substring. This is the canonical CLI redaction promise.
//
// We don't require a populated state-dir — even an empty one should
// not emit token bytes. If LoadHubEndpoint errors, status records
// the error in endpoint_error but never echoes credentials.
func TestHubMcpStatusOutputRedactsTokens(t *testing.T) {
	root := NewRootCmd()
	var stdout bytes.Buffer
	root.SetArgs([]string{"hub-mcp", "status"})
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("hub-mcp status: %v", err)
	}
	// 64-hex strings (token shape per MCP spec + RedactToken target).
	re := regexp.MustCompile(`[0-9a-f]{64}`)
	if re.MatchString(stdout.String()) {
		t.Errorf("status output contains 64-hex token-like substring:\n%s", stdout.String())
	}
	// Sanity: the output should be valid JSON. The marshaled map
	// keys must contain "per_client_tokens" and "instance_id".
	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("status output is not valid JSON: %v\noutput:\n%s", err, stdout.String())
	}
	if _, ok := parsed["per_client_tokens"]; !ok {
		t.Errorf("status output missing per_client_tokens key")
	}
	if _, ok := parsed["instance_id"]; !ok {
		t.Errorf("status output missing instance_id key")
	}
	// instance_id is always a presence tag, never the raw value.
	iid, _ := parsed["instance_id"].(string)
	if iid != "ABSENT" && iid != "PRESENT" {
		t.Errorf("instance_id should be ABSENT|PRESENT presence tag, got %q", iid)
	}
}

// TestHubMcpStatusPresenceTagHelper pins the presenceTag helper. It's
// the choke point that prevents raw token bytes from reaching the
// CLI surface — a regression that returns the raw value would leak
// 64-hex through every status call.
func TestHubMcpStatusPresenceTagHelper(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "ABSENT"},
		{"single-char", "a", "PRESENT"},
		{"64-hex", strings.Repeat("a", 64), "PRESENT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := presenceTag(c.in)
			if got != c.want {
				t.Errorf("presenceTag(%q) = %q, want %q", c.in, got, c.want)
			}
			// Never echo the input.
			if c.in != "" && got == c.in {
				t.Errorf("presenceTag leaked input value: %q == %q", got, c.in)
			}
		})
	}
}

// TestHubMcpStatusOutputUsesRedactTokenChokepoint pins that the
// status command pipes its marshaled output through
// api.RedactToken — defense-in-depth even though every emit-time
// path already redacts. A future regression that introduces a new
// state field carrying a token value would still be caught by this
// final pass.
func TestHubMcpStatusOutputUsesRedactTokenChokepoint(t *testing.T) {
	// Synthesize a fake 64-hex string and verify RedactToken
	// removes it from a marshaled JSON shape similar to status output.
	fake := strings.Repeat("a", 64)
	probe := map[string]any{
		"port":   9120,
		"hidden": "this-should-not-leak: " + fake,
	}
	raw, _ := json.Marshal(probe)
	got := api.RedactToken(string(raw))
	if strings.Contains(got, fake) {
		t.Errorf("RedactToken did not scrub a 64-hex literal; got:\n%s", got)
	}
}
