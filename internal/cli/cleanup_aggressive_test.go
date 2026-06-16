package cli

import (
	"bytes"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestCleanupAggressive_RejectsWithoutScope: `mcphub cleanup aggressive`
// with neither --client nor --root-pid exits non-zero with an explicit
// "scope required" message (spec H.1 acceptance criterion 1).
func TestCleanupAggressive_RejectsWithoutScope(t *testing.T) {
	cmd := newCleanupAggressiveCmdReal()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no scope is set")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Fatalf("expected a scope-required message, got: %v", err)
	}
}

// TestCleanupAggressive_RejectsBothScopes: setting both --client and
// --root-pid is rejected (exactly one scope).
func TestCleanupAggressive_RejectsBothScopes(t *testing.T) {
	cmd := newCleanupAggressiveCmdReal()
	cmd.SetArgs([]string{"--client", "codex", "--root-pid", "1234"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when both scopes are set")
	}
}

// TestCleanupAggressive_RejectsUnknownClient: an unrecognized client name
// is rejected loudly.
func TestCleanupAggressive_RejectsUnknownClient(t *testing.T) {
	cmd := newCleanupAggressiveCmdReal()
	cmd.SetArgs([]string{"--client", "definitely-not-a-client"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for an unknown client launcher")
	}
}

// TestCleanupAggressive_IncludeClassFlagOverridesWithWarning: passing
// --include-class emits a stderr warning naming the dangerous class
// (spec H.1). The warning fires before the platform-specific snapshot, so
// this is portable. A valid scope (--client codex) is supplied so the
// command reaches the warning emit.
func TestCleanupAggressive_IncludeClassFlagOverridesWithWarning(t *testing.T) {
	cmd := newCleanupAggressiveCmdReal()
	cmd.SetArgs([]string{"--client", "codex", "--include-class", "chrome"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stderr.String(), "chrome") || !strings.Contains(strings.ToLower(stderr.String()), "warning") {
		t.Fatalf("expected a stderr warning naming the opted-in class 'chrome', got stderr=%q", stderr.String())
	}
}

// TestAggressiveConfirmToken_DeterministicAndSetBound verifies the token
// is stable for an unchanged candidate set, order-independent, and
// changes when the set changes (the stale-token rejection contract).
func TestAggressiveConfirmToken_DeterministicAndSetBound(t *testing.T) {
	set := []api.OrphanProcess{
		{PID: 100, CmdlineDisplay: "node.exe", MatchSource: "codex"},
		{PID: 200, CmdlineDisplay: "python.exe", MatchSource: "codex"},
	}
	t1 := aggressiveConfirmToken(set)

	// Reordering the same members yields the same token (sort-stable).
	reordered := []api.OrphanProcess{set[1], set[0]}
	if t2 := aggressiveConfirmToken(reordered); t2 != t1 {
		t.Errorf("token must be order-independent: %q != %q", t1, t2)
	}

	// Adding a member changes the token (stale token would be rejected).
	added := append(append([]api.OrphanProcess{}, set...),
		api.OrphanProcess{PID: 300, CmdlineDisplay: "uv.exe", MatchSource: "codex"})
	if t3 := aggressiveConfirmToken(added); t3 == t1 {
		t.Errorf("token must change when a candidate is added; both = %q", t1)
	}

	// Changing a PID's identity (different exe) changes the token.
	mutated := []api.OrphanProcess{
		{PID: 100, CmdlineDisplay: "node.exe", MatchSource: "codex"},
		{PID: 200, CmdlineDisplay: "OTHER.exe", MatchSource: "codex"},
	}
	if t4 := aggressiveConfirmToken(mutated); t4 == t1 {
		t.Errorf("token must change when a candidate's identity changes; both = %q", t1)
	}

	if len(t1) != 16 {
		t.Errorf("token should be 16 hex chars, got %d (%q)", len(t1), t1)
	}
}

// TestAggressiveSkippedClasses_ReportsRemainingDenyList verifies the audit
// helper reports the deny-list minus operator opt-ins.
func TestAggressiveSkippedClasses_ReportsRemainingDenyList(t *testing.T) {
	skipped := aggressiveSkippedClasses([]string{"chrome"})
	for _, c := range skipped {
		if c == "chrome" {
			t.Errorf("chrome was opted in via --include-class; it must NOT appear in skipped_classes")
		}
	}
	// cmd was not opted in, so it must still be reported as skipped.
	found := false
	for _, c := range skipped {
		if c == "cmd" {
			found = true
		}
	}
	if !found {
		t.Errorf("cmd was not opted in; it must appear in skipped_classes, got %v", skipped)
	}
}
