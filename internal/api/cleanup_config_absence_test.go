package api

import (
	"context"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
)

// swapScanClientConfigsFailClosed injects a fake config-reference substrate for the
// H5 config-absence gate so the gate is unit-testable without real client configs.
func swapScanClientConfigsFailClosed(t *testing.T, fn func() (patterns []string, degradedClients []string)) {
	t.Helper()
	prev := scanClientConfigsFailClosedFn
	scanClientConfigsFailClosedFn = fn
	t.Cleanup(func() { scanClientConfigsFailClosedFn = prev })
}

func TestCandidateConfigReferenced(t *testing.T) {
	o := OrphanProcess{Cmdline: `node npx -y @mui/mcp --port 9200`}
	if !candidateConfigReferenced(o, []string{"@mui/mcp"}) {
		t.Error("a present discriminating config pattern must count as referenced (→ spare)")
	}
	if candidateConfigReferenced(o, []string{"@other/pkg"}) {
		t.Error("an absent pattern must NOT count as referenced (→ reap-eligible)")
	}
	// A broad launcher token (node) must never spare — otherwise every node process
	// with any config installed would be un-reapable.
	if candidateConfigReferenced(o, []string{"node"}) {
		t.Error("a broad launcher token must never count as a config reference")
	}
	if candidateConfigReferenced(o, nil) {
		t.Error("no config patterns → not referenced")
	}
}

// TestApplyReapEligibilityGate_ReferencedCandidateNeverReachesKill is the headline
// regression guard (bug 2026-07-08 A2 PR5 / H5 T1-hole): a candidate whose signature
// is still referenced by an installed client config is SPARED and NEVER reaches the
// kill primitive, even on apply (DryRun=false).
func TestApplyReapEligibilityGate_ReferencedCandidateNeverReachesKill(t *testing.T) {
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return []string{"@mui/mcp"}, nil })
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { calls = append(calls, p); return nil })

	filtered := []OrphanProcess{{PID: 1234, Cmdline: `node npx -y @mui/mcp`, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	applyReapEligibilityGate(filtered, false /*apply*/, false)

	if filtered[0].ReapVerdict != ReapVerdictSparedConfigReferenced {
		t.Errorf("referenced candidate verdict = %q, want %q", filtered[0].ReapVerdict, ReapVerdictSparedConfigReferenced)
	}
	if len(calls) != 0 {
		t.Errorf("a config-referenced candidate must NEVER reach the kill primitive; got %d terminate call(s)", len(calls))
	}
}

// TestApplyReapEligibilityGate_ConfigAbsentIsReapEligibleAndKills: a candidate whose
// signature is absent from every parseable client config is reap-eligible and IS killed
// on apply (proves the gate does not neuter the positive path).
func TestApplyReapEligibilityGate_ConfigAbsentIsReapEligibleAndKills(t *testing.T) {
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return []string{"@other/pkg"}, nil })
	const cmdline = `node npx -y @mui/mcp`
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { calls = append(calls, p); return nil })

	filtered := []OrphanProcess{{PID: 1234, AgeSec: 700, Cmdline: cmdline, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	applyReapEligibilityGate(filtered, false, false)

	if filtered[0].ReapVerdict != ReapVerdictReapEligible {
		t.Errorf("config-absent candidate verdict = %q, want %q", filtered[0].ReapVerdict, ReapVerdictReapEligible)
	}
	if len(calls) != 1 {
		t.Errorf("a reap-eligible candidate must reach the kill primitive exactly once; got %d", len(calls))
	}
}

// TestApplyReapEligibilityGate_DegradedConfigSparesEveryCandidate: if ANY existing
// client config could not be parsed, the reaper cannot prove absence, so EVERY
// otherwise-eligible candidate this run is spared (fail closed), with the degraded
// client named (never a path) in KillErr.
func TestApplyReapEligibilityGate_DegradedConfigSparesEveryCandidate(t *testing.T) {
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return nil, []string{"cursor"} })
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { calls = append(calls, p); return nil })

	filtered := []OrphanProcess{{PID: 1234, Cmdline: `node npx -y @mui/mcp`, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	applyReapEligibilityGate(filtered, false, false)

	if filtered[0].ReapVerdict != ReapVerdictSparedConfigScanDegraded {
		t.Errorf("degraded-scan verdict = %q, want %q", filtered[0].ReapVerdict, ReapVerdictSparedConfigScanDegraded)
	}
	if len(calls) != 0 {
		t.Errorf("a degraded config scan must spare ALL candidates (fail closed); got %d kill(s)", len(calls))
	}
	if !strings.Contains(filtered[0].KillErr, "cursor") {
		t.Errorf("degraded KillErr must name the degraded client (names only); got %q", filtered[0].KillErr)
	}
	if strings.Contains(filtered[0].KillErr, `\`) || strings.Contains(filtered[0].KillErr, "/") {
		t.Errorf("degraded KillErr must never carry a path; got %q", filtered[0].KillErr)
	}
}

// TestApplyReapEligibilityGate_DryRunNeverKills: a dry-run stamps verdicts but performs
// zero kills.
func TestApplyReapEligibilityGate_DryRunNeverKills(t *testing.T) {
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return nil, nil }) // eligible
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { calls = append(calls, p); return nil })

	filtered := []OrphanProcess{{PID: 1234, AgeSec: 700, Cmdline: `node npx -y @mui/mcp`, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	applyReapEligibilityGate(filtered, true /*dryRun*/, false)

	if filtered[0].ReapVerdict != ReapVerdictReapEligible {
		t.Errorf("dry-run must still stamp the eligibility verdict; got %q", filtered[0].ReapVerdict)
	}
	if len(calls) != 0 {
		t.Errorf("a dry-run must never kill; got %d", len(calls))
	}
}

// TestApplyReapEligibilityGate_DryRunLeavesKillErrEmpty is the direct regression guard for
// the PR #520 P1 consent-surface bug: a dry-run stamps the structured ReapVerdict but must
// leave the human KillErr EMPTY on every row (spared AND eligible). The GUI renders
// `kill_err || "killed"` and shows a result column once ANY row carries kill_err, so a
// dry-run that stamped spare-reasons would render a reap-ELIGIBLE row's empty kill_err as a
// false "killed" alongside a spared row's reason — before the operator pressed Clean.
func TestApplyReapEligibilityGate_DryRunLeavesKillErrEmpty(t *testing.T) {
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return nil, nil })
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { t.Fatalf("dry-run must not kill"); return nil })

	filtered := []OrphanProcess{
		{PID: 1, AgeSec: 100, Cmdline: `node npx -y @mui/mcp`, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"},   // below floor → spared
		{PID: 2, AgeSec: 700, Cmdline: `node npx -y @other/pkg`, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}, // config-absent + old → eligible
	}
	applyReapEligibilityGate(filtered, true /*dryRun*/, false)

	if filtered[0].ReapVerdict != ReapVerdictSparedBelowKillAgeFloor {
		t.Errorf("below-floor dry-run verdict = %q, want %q", filtered[0].ReapVerdict, ReapVerdictSparedBelowKillAgeFloor)
	}
	if filtered[1].ReapVerdict != ReapVerdictReapEligible {
		t.Errorf("above-floor dry-run verdict = %q, want %q", filtered[1].ReapVerdict, ReapVerdictReapEligible)
	}
	for i := range filtered {
		if filtered[i].KillErr != "" {
			t.Errorf("dry-run must leave KillErr EMPTY (GUI renders kill_err||\"killed\"); row %d got %q", i, filtered[i].KillErr)
		}
	}
}

// TestReapOrphans_AggressivePathNotConfigGated pins the S13 scope boundary: reapOrphans
// (AggressiveCleanup's kill path) must NOT inherit the config-absence gate — it kills a
// candidate regardless of whether its signature is still config-referenced (the aggressive
// operator-scoped kill contract), because a live client's config is definitionally present.
func TestReapOrphans_AggressivePathNotConfigGated(t *testing.T) {
	// Even though this signature WOULD be spared under CleanupOrphans's gate...
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return []string{"@mui/mcp"}, nil })
	const cmdline = `node npx -y @mui/mcp`
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { calls = append(calls, p); return nil })

	filtered := []OrphanProcess{{PID: 1234, Cmdline: cmdline, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	reapOrphans(filtered, false) // the AggressiveCleanup path

	if len(calls) != 1 {
		t.Errorf("AggressiveCleanup's reapOrphans must NOT be config-gated; want 1 kill, got %d", len(calls))
	}
}

// TestApplyReapEligibilityGate_SnapshotDegradedSparesEveryCandidate ($security-reviewer
// P1): a truncated process snapshot can drop a live-ancestor row and mis-classify a
// live-rooted child as an orphan, so the ancestor-walk verdict is untrustworthy — spare
// EVERY candidate (fail closed), even a config-absent one that would otherwise be eligible.
func TestApplyReapEligibilityGate_SnapshotDegradedSparesEveryCandidate(t *testing.T) {
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return nil, nil }) // would be eligible
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { calls = append(calls, p); return nil })

	filtered := []OrphanProcess{{PID: 1234, AgeSec: 99999, Cmdline: `node npx -y @mui/mcp`, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	applyReapEligibilityGate(filtered, false /*apply*/, true /*snapshotDegraded*/)

	if filtered[0].ReapVerdict != ReapVerdictSparedSnapshotDegraded {
		t.Errorf("snapshot-degraded verdict = %q, want %q", filtered[0].ReapVerdict, ReapVerdictSparedSnapshotDegraded)
	}
	if len(calls) != 0 {
		t.Errorf("a truncated snapshot must spare ALL candidates (fail closed); got %d kill(s)", len(calls))
	}
}

// TestApplyReapEligibilityGate_BelowKillAgeFloorSparesButListed: a candidate younger than
// the 600s kill-age floor is LISTED (verdict) but NOT killed on apply.
func TestApplyReapEligibilityGate_BelowKillAgeFloorSparesButListed(t *testing.T) {
	swapScanClientConfigsFailClosed(t, func() ([]string, []string) { return nil, nil }) // config-absent
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, func(p process.PIDIdentityProof) error { calls = append(calls, p); return nil })

	young := []OrphanProcess{{PID: 1, AgeSec: 120, Cmdline: `node npx -y @mui/mcp`, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	applyReapEligibilityGate(young, false, false)
	if young[0].ReapVerdict != ReapVerdictSparedBelowKillAgeFloor {
		t.Errorf("120s candidate verdict = %q, want %q", young[0].ReapVerdict, ReapVerdictSparedBelowKillAgeFloor)
	}
	if len(calls) != 0 {
		t.Errorf("a below-floor candidate must not be killed; got %d", len(calls))
	}

	// An old candidate clears the floor and is killed.
	const cmdline = `node npx -y @mui/mcp`
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	old := []OrphanProcess{{PID: 2, AgeSec: 700, Cmdline: cmdline, ExecutablePath: `C:\node.exe`, StartedAt: "2026-01-01T00:00:00Z"}}
	applyReapEligibilityGate(old, false, false)
	if old[0].ReapVerdict != ReapVerdictReapEligible {
		t.Errorf("700s candidate verdict = %q, want %q", old[0].ReapVerdict, ReapVerdictReapEligible)
	}
	if len(calls) != 1 {
		t.Errorf("an above-floor config-absent candidate must be killed once; got %d", len(calls))
	}
}

// TestPatternsFromManifestExFallbackFlag pins the T3 bare-name demotion at its SOURCE
// ($security-reviewer P2 + bot PR #520 P2): patternsFromManifestEx must flag fallback=true
// ONLY when the manifest yields no discriminating command/arg and collapses to the bare
// server name — and flag fallback=false for a REAL manifest whose command basename
// legitimately equals the server name (e.g. the shipped `mcp-language-server`). Getting
// this wrong in either direction is a bug: a false positive drops a real self-named server
// (the bot regression); a false negative nominates a bare generic word for the auto-ticker.
func TestPatternsFromManifestExFallbackFlag(t *testing.T) {
	cases := []struct {
		name         string
		server       string
		m            config.ServerManifest
		wantFallback bool
		wantContains string // a pattern that MUST be present
	}{
		{"empty manifest collapses to bare name", "memory", config.ServerManifest{}, true, "memory"},
		{"real self-named binary is NOT a fallback", "mcp-language-server",
			config.ServerManifest{Command: "mcp-language-server"}, false, "mcp-language-server"},
		{"real command+arg manifest", "wolfram",
			config.ServerManifest{Command: "wolfram-server", BaseArgs: []string{"@wolf/pkg"}}, false, "wolfram-server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pats, fallback := patternsFromManifestEx(tc.server, &tc.m)
			if fallback != tc.wantFallback {
				t.Errorf("fallback = %v, want %v (pats %v)", fallback, tc.wantFallback, pats)
			}
			found := false
			for _, p := range pats {
				if p == tc.wantContains {
					found = true
				}
			}
			if !found {
				t.Errorf("pattern %q must be present; got %v", tc.wantContains, pats)
			}
		})
	}
}

// TestManifestNominationPreservesRealSelfNamedPattern is the direct regression guard for
// bot PR #520 P2: manifestNominationPatterns must NOT drop a real self-named pattern like
// `mcp-language-server` (the source-level nominatable gate already discarded true bare-name
// fallbacks, so anything reaching this flattener is real). It still drops broad launcher
// tokens.
func TestManifestNominationPreservesRealSelfNamedPattern(t *testing.T) {
	patterns := map[string][]string{
		"mcp-language-server": {"mcp-language-server"},   // real self-named binary → kept
		"wolfram":             {"wolfram-server", "npx"}, // real + a broad launcher token
	}
	got := manifestNominationPatterns(patterns)
	has := func(want string) bool {
		for _, p := range got {
			if p == want {
				return true
			}
		}
		return false
	}
	if !has("mcp-language-server") {
		t.Errorf("real self-named pattern must survive nomination; got %v", got)
	}
	if !has("wolfram-server") {
		t.Errorf("real discriminating pattern must survive nomination; got %v", got)
	}
	if has("npx") {
		t.Errorf("broad launcher token must be dropped from nomination; got %v", got)
	}
}
