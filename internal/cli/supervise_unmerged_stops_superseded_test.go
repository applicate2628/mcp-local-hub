// Package cli — bot PR #288 r34 P2 regression test for the
// hasUnmergedActiveLegacyStops startup fail-closed gate.
//
// The gate decides whether supervisor STARTUP must fail closed after a
// daemon-intent collapse error because a valid legacy daemon-intent.json
// stop is not yet durably represented in supervisor-intent.json's stops
// sub-block. Before this fix the gate required BYTE-EQUIVALENCE between
// the sub-block record and the legacy record. But the authoritative
// collapse delete-gate in internal/api/intent_collapse.go:592-593
// (daemonIntentRecordMergedOrSuperseded) already treats a sub-block stop
// whose UpdatedAt is strictly AFTER the legacy UpdatedAt as
// merged/superseded — the merge keeps the newer sub-block authority. So
// when the sub-block legitimately SUPERSEDES the legacy stop (newer
// UpdatedAt, possibly different reason), the old byte-equivalence gate
// saw `!UpdatedAt.Equal` (and/or a reason mismatch) and returned true,
// permanently fail-closing startup on EVERY boot.
//
// These tests pin the corrected equal-or-strictly-newer logic and its
// negative controls. The function is a pure predicate; the structs are
// built in memory with no disk, schtasks, IPC, or port binding.
package cli

import (
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestHasUnmergedActiveLegacyStops_NewerSubBlockSupersedesLegacy is the
// falsifying regression for bot PR #288 r34 P2. A sub-block stop with a
// strictly-NEWER UpdatedAt and a DIFFERENT reason than the legacy stop is
// merged/superseded and must NOT trip the fail-closed gate. Pre-fix the
// byte-equivalence check returned true here (reason mismatch + !Equal),
// fail-closing startup forever.
func TestHasUnmergedActiveLegacyStops_NewerSubBlockSupersedesLegacy(t *testing.T) {
	now := time.Now().UTC()

	// Legacy stop: user-stop recorded an hour ago — still an active stop
	// (within the 24h user-stop TTL).
	legacyStop := api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUserStop,
		UpdatedAt: now.Add(-1 * time.Hour),
	}
	// Sub-block stop: strictly NEWER UpdatedAt AND a different reason
	// (user-disabled). user-disabled stays an active stop past the TTL, and
	// at `now` it is well within window. The differing reason proves
	// byte-equivalence is NOT required for the merged/superseded verdict.
	subBlockStop := api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUserDisabled,
		UpdatedAt: now,
	}

	supervisorIntent := &api.SupervisorIntentFile{
		Version: 1,
		Stops:   map[string]api.DaemonIntent{`\mcp-local-hub-foo-default`: subBlockStop},
	}
	legacy := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{`\mcp-local-hub-foo-default`: legacyStop},
	}

	if hasUnmergedActiveLegacyStops(supervisorIntent, legacy, now) {
		t.Fatalf("a strictly-newer sub-block stop (different reason) authoritatively supersedes the legacy stop and must NOT read as unmerged; mirrors intent_collapse.go daemonIntentRecordMergedOrSuperseded (would permanently fail-close startup)")
	}
}

// TestHasUnmergedActiveLegacyStops_SupersededNegativeControls pins the
// three remaining cases so the fix cannot silently un-gate genuinely
// unmerged stops.
func TestHasUnmergedActiveLegacyStops_SupersededNegativeControls(t *testing.T) {
	now := time.Now().UTC()

	legacyStop := api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUserStop,
		UpdatedAt: now.Add(-1 * time.Hour),
	}
	legacy := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{`\mcp-local-hub-foo-default`: legacyStop},
	}

	t.Run("sub-block ABSENT trips the gate", func(t *testing.T) {
		supervisorIntent := &api.SupervisorIntentFile{
			Version: 1,
			Stops:   map[string]api.DaemonIntent{`\mcp-local-hub-bar-default`: legacyStop},
		}
		if !hasUnmergedActiveLegacyStops(supervisorIntent, legacy, now) {
			t.Fatalf("an active legacy stop with NO sub-block entry under its canonical key must trip the fail-closed gate")
		}
	})

	t.Run("sub-block OLDER UpdatedAt trips the gate", func(t *testing.T) {
		olderSubBlock := api.DaemonIntent{
			Desired:   api.IntentDesiredStopped,
			Reason:    api.IntentReasonUserStop,
			UpdatedAt: now.Add(-2 * time.Hour), // older than the legacy stop
		}
		supervisorIntent := &api.SupervisorIntentFile{
			Version: 1,
			Stops:   map[string]api.DaemonIntent{`\mcp-local-hub-foo-default`: olderSubBlock},
		}
		if !hasUnmergedActiveLegacyStops(supervisorIntent, legacy, now) {
			t.Fatalf("a sub-block stop OLDER than the legacy stop is neither equal nor strictly newer and must trip the fail-closed gate")
		}
	})

	t.Run("sub-block EQUAL does not trip the gate", func(t *testing.T) {
		supervisorIntent := &api.SupervisorIntentFile{
			Version: 1,
			Stops:   map[string]api.DaemonIntent{`\mcp-local-hub-foo-default`: legacyStop}, // identical record
		}
		if hasUnmergedActiveLegacyStops(supervisorIntent, legacy, now) {
			t.Fatalf("a byte-equivalent sub-block stop is merged and must NOT trip the fail-closed gate (unchanged pre/post-fix)")
		}
	})
}
