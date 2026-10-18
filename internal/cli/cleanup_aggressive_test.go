package cli

import (
	"bytes"
	"runtime"
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
// --include-class retains its warning on the non-Windows empty-success path.
// Windows refuses missing retirement authority before warnings or success output.
func TestCleanupAggressive_IncludeClassFlagOverridesWithWarning(t *testing.T) {
	cmd := newCleanupAggressiveCmdReal()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--client", "codex", "--include-class", "chrome"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	if runtime.GOOS == "windows" {
		const cause = "aggressive cleanup unavailable: creator retirement authority is not established; preview and apply disabled"
		if err == nil || err.Error() != cause {
			t.Fatalf("error = %v, want retirement-authority refusal", err)
		}
		if stdout.Len() != 0 || strings.Contains(strings.ToLower(stderr.String()), "warning:") {
			t.Fatalf("refusal emitted success or class warning: stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stderr.String(), "chrome") || !strings.Contains(strings.ToLower(stderr.String()), "warning") {
		t.Fatalf("expected a stderr warning naming the opted-in class 'chrome', got stderr=%q", stderr.String())
	}
}

func TestCleanupAggressive_RetirementAuthorityUnavailable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows retirement-authority containment")
	}
	const cause = "aggressive cleanup unavailable: creator retirement authority is not established; preview and apply disabled"
	oldStateDir := stateDirFunc
	stateDirCalls := 0
	stateDirFunc = func() (string, error) {
		stateDirCalls++
		return "", nil
	}
	t.Cleanup(func() { stateDirFunc = oldStateDir })
	for _, args := range [][]string{
		{"--client", "codex"},
		{"--root-pid", "1234", "--confirm-aggressive-token", "synthetic-old-token", "--include-class", "chrome", "--min-age-sec", "1"},
	} {
		cmd := newCleanupAggressiveCmdReal()
		cmd.SetArgs(args)
		cmd.SilenceUsage = true
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := cmd.Execute(); err == nil || err.Error() != cause {
			t.Fatalf("args=%v error=%v, want fixed refusal", args, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("refusal published output: %q", stdout.String())
		}
	}
	if stateDirCalls != 0 {
		t.Fatalf("success audit requested state directory %d times", stateDirCalls)
	}
}

func TestCleanupAggressive_HelpStatesUnavailability(t *testing.T) {
	cmd := newCleanupAggressiveCmdReal()
	if !strings.Contains(cmd.Long, "preview and apply are disabled") ||
		!strings.Contains(cmd.Long, "cannot enable this operation") {
		t.Fatalf("help must explain current availability and confirmation limits: %s", cmd.Long)
	}
}

// TestAggressiveConfirmToken_DeterministicAndSetBound verifies the token
// is stable for an unchanged candidate set, order-independent, and
// changes when the set changes (the stale-token rejection contract). The
// token fn now lives in the api package (single owner shared by the CLI
// and the GUI apply path); this test exercises api.AggressiveConfirmToken
// directly to prove the CLI-relied-on contract still holds after the move.
func TestAggressiveConfirmToken_DeterministicAndSetBound(t *testing.T) {
	set := []api.OrphanProcess{
		{PID: 100, CmdlineDisplay: "node.exe", MatchSource: "codex"},
		{PID: 200, CmdlineDisplay: "python.exe", MatchSource: "codex"},
	}
	t1 := api.AggressiveConfirmToken(set)

	// Reordering the same members yields the same token (sort-stable).
	reordered := []api.OrphanProcess{set[1], set[0]}
	if t2 := api.AggressiveConfirmToken(reordered); t2 != t1 {
		t.Errorf("token must be order-independent: %q != %q", t1, t2)
	}

	// Adding a member changes the token (stale token would be rejected).
	added := append(append([]api.OrphanProcess{}, set...),
		api.OrphanProcess{PID: 300, CmdlineDisplay: "uv.exe", MatchSource: "codex"})
	if t3 := api.AggressiveConfirmToken(added); t3 == t1 {
		t.Errorf("token must change when a candidate is added; both = %q", t1)
	}

	// Changing a PID's identity (different exe) changes the token.
	mutated := []api.OrphanProcess{
		{PID: 100, CmdlineDisplay: "node.exe", MatchSource: "codex"},
		{PID: 200, CmdlineDisplay: "OTHER.exe", MatchSource: "codex"},
	}
	if t4 := api.AggressiveConfirmToken(mutated); t4 == t1 {
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
