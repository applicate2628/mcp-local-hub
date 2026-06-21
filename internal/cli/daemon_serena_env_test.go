package cli

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/secrets"
)

// These tests cover the serena-proxy-ignores-env-overlay fix: the serena
// per-workspace proxy now merges the operator env overlay onto its resolved
// EnvRefs through the SAME task-name-keyed owner the global daemon path uses
// (mergeResolvedDaemonEnvWithOverlay), keyed by the descriptor's authoritative
// --task-name (workspace-keyed via SerenaTaskNameForWorkspace), not by
// server/daemon. They are STATE-SAFE: every test redirects the state dir via
// MCPHUB_STATE_DIR_OVERRIDE to a hardened temp dir, so a full `go test` can
// never touch the live supervisor-intent.json / overlay file.

// seedSerenaOverlay writes an operator overlay row keyed by an arbitrary
// task name (so a test can seed either the canonical serena task name or a
// deliberately-wrong one), mirroring seedDaemonOverlay's pattern.
func seedSerenaOverlay(t *testing.T, stateDir, taskName string, env map[string]string) {
	t.Helper()
	if err := daemon_env_overlay.WriteOverlay(filepath.Join(stateDir, overlayBaseName), func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[taskName] = daemon_env_overlay.DaemonRow{
			Source: "operator",
			Env:    env,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed serena overlay: %v", err)
	}
}

// TestSerenaProxyEnvOverlayReachesChildAndOperatorWins is the core
// serena-proxy-ignores-env-overlay regression guard. A supervisor-spawned
// serena proxy inherits the operator overlay value in os.Environ() (the marker
// is set) AND its EnvRefs resolve a value for the SAME overlap key. Before the
// fix the serena proxy populated the child env from ResolveMapBestEffort ALONE,
// so the operator override (SERENA_LOG_LEVEL) was silently dropped and the
// EnvRefs overlap value clobbered the overlay. The fix routes through the
// shared task-name-keyed owner so the overlay value WINS in cfg.Env.
func TestSerenaProxyEnvOverlayReachesChildAndOperatorWins(t *testing.T) {
	const canonicalWS = `D:\dev\some-workspace`
	taskName := api.SerenaTaskNameForWorkspace(canonicalWS)

	const operatorLogLevel = "operator-value"
	const operatorOverlap = "operator-overlap"
	const manifestOverlap = "manifest-value"
	const overlapKey = "SERENA_PROJECT_ROOT"

	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	// serena-proxy is ALWAYS supervisor-spawned: set the marker so the overlay
	// owner reads the supervisor-applied (already-expanded) overlay values back
	// from os.Environ(), mirroring the global daemon supervisor-applied path.
	t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
	// The supervisor merged these overlay values into THIS wrapper's
	// environment (overlay-wins) before spawning us; they are literal values
	// (no ${parent_path}), so os.Environ() carries them verbatim.
	t.Setenv("SERENA_LOG_LEVEL", operatorLogLevel)
	t.Setenv(overlapKey, operatorOverlap)
	// Overlay row keyed by the canonical serena task name (workspace-keyed),
	// carrying both the operator-only key and the overlap key.
	seedSerenaOverlay(t, stateDir, taskName, map[string]string{
		"SERENA_LOG_LEVEL": operatorLogLevel,
		overlapKey:         operatorOverlap,
	})

	// The resolved EnvRefs carry the overlap key with the MANIFEST value — the
	// overlay must win over it.
	resolved := map[string]string{
		overlapKey: manifestOverlap,
	}
	merged, _, err := mergeResolvedDaemonEnvWithOverlay(taskName, resolved, nil)
	if err != nil {
		t.Fatalf("mergeResolvedDaemonEnvWithOverlay: %v", err)
	}

	// (a) The operator-only key REACHES the child env.
	if got := merged["SERENA_LOG_LEVEL"]; got != operatorLogLevel {
		t.Fatalf("merged SERENA_LOG_LEVEL = %q, want operator overlay value %q (overlay dropped — serena-proxy-ignores-env-overlay regressed)", got, operatorLogLevel)
	}
	// (b) For the overlap key the OPERATOR value wins over the EnvRefs value in
	// cfg.Env so the upstream child (append(os.Environ(), cfg.Env...)) sees it.
	if got := merged[overlapKey]; got != operatorOverlap {
		t.Fatalf("merged %s = %q, want operator overlay value %q (manifest %q must not clobber)", overlapKey, got, operatorOverlap, manifestOverlap)
	}
	// (c) Prove the EFFECTIVE value the host passes to the upstream child.
	if got := effectiveChildValueFromEnvMap(os.Environ(), merged, "SERENA_LOG_LEVEL"); got != operatorLogLevel {
		t.Fatalf("effective child SERENA_LOG_LEVEL = %q, want operator overlay value %q", got, operatorLogLevel)
	}
	if got := effectiveChildValueFromEnvMap(os.Environ(), merged, overlapKey); got != operatorOverlap {
		t.Fatalf("effective child %s = %q, want operator overlay value %q", overlapKey, got, operatorOverlap)
	}
}

// TestSerenaProxyEnvNoOverlayRowIsByteUnchanged proves that with NO overlay row
// for this serena task name, the merged child env is exactly the resolved
// EnvRefs map — the overlay merge is a no-op so a workspace with no operator
// override behaves identically to a plain ResolveMapBestEffort.
func TestSerenaProxyEnvNoOverlayRowIsByteUnchanged(t *testing.T) {
	const canonicalWS = `D:\dev\no-overlay-workspace`
	taskName := api.SerenaTaskNameForWorkspace(canonicalWS)

	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
	// Seed an overlay file with an UNRELATED row so Load succeeds but no row
	// matches THIS task name (LookupOverlay → nil → no-op merge).
	seedSerenaOverlay(t, stateDir, `\mcp-local-hub-other-default`, map[string]string{
		"UNRELATED": "value",
	})

	resolved, omitted, err := secrets.NewResolver(nil, nil).ResolveMapBestEffort(map[string]string{
		"SERENA_LOG_LEVEL": "info",
		"SERENA_DOCKER":     "0",
	})
	if err != nil {
		t.Fatalf("ResolveMapBestEffort: %v", err)
	}

	merged, unset, err := mergeResolvedDaemonEnvWithOverlay(taskName, resolved, omitted)
	if err != nil {
		t.Fatalf("mergeResolvedDaemonEnvWithOverlay: %v", err)
	}
	if len(unset) != 0 {
		t.Fatalf("unset = %v, want empty (no omitted secrets)", unset)
	}
	if len(merged) != len(resolved) {
		t.Fatalf("merged has %d keys, want exactly the resolved %d keys (no-overlay merge must be a no-op)", len(merged), len(resolved))
	}
	for k, want := range resolved {
		if got := merged[k]; got != want {
			t.Fatalf("merged[%q] = %q, want resolved value %q byte-unchanged", k, got, want)
		}
	}
}

// TestSerenaProxyEnvWrongKeyFormatDoesNotMatch is the task-name-keying guard.
// An overlay row keyed by the OLD server-daemon format
// (`\mcp-local-hub-serena-default`) must NOT match a serena proxy whose
// authoritative task name is workspace-keyed
// (`\mcp-local-hub-serena-<wskey>`). This is the bug class the parameterization
// fixes: the owner must key on the descriptor's --task-name, never reconstruct
// a server-daemon name that would load a phantom (or wrong) row.
func TestSerenaProxyEnvWrongKeyFormatDoesNotMatch(t *testing.T) {
	const canonicalWS = `D:\dev\wrong-key-workspace`
	taskName := api.SerenaTaskNameForWorkspace(canonicalWS)

	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
	// Operator value present in os.Environ() (as if the supervisor had applied
	// it) so a FALSE match would actually surface in the merged env.
	t.Setenv("SERENA_LOG_LEVEL", "should-not-be-picked-up")
	// Row keyed by the WRONG (server-daemon) format — NOT the workspace-keyed
	// task name the descriptor carries.
	seedSerenaOverlay(t, stateDir, `\mcp-local-hub-serena-default`, map[string]string{
		"SERENA_LOG_LEVEL": "should-not-be-picked-up",
	})

	resolved := map[string]string{
		"SERENA_DOCKER": "0",
	}
	merged, _, err := mergeResolvedDaemonEnvWithOverlay(taskName, resolved, nil)
	if err != nil {
		t.Fatalf("mergeResolvedDaemonEnvWithOverlay: %v", err)
	}
	if _, ok := merged["SERENA_LOG_LEVEL"]; ok {
		t.Fatalf("merged carries SERENA_LOG_LEVEL from a wrong-format overlay row %q; the owner must key on the workspace task name %q, not a reconstructed server-daemon name", `\mcp-local-hub-serena-default`, taskName)
	}
	// The resolved env passes through untouched.
	if got := merged["SERENA_DOCKER"]; got != "0" {
		t.Fatalf("merged SERENA_DOCKER = %q, want resolved value %q", got, "0")
	}
}

// TestNoRowSupervisedDaemonSurvivesMalformedOverlay is the PR #403 bot-edge
// end-to-end brick guard. A supervised daemon with NO overlay row (marker
// present, but the supervisor injected an EMPTY overlay key set because this
// daemon had no overlay) must NOT fail its launch when an UNRELATED operator
// edit corrupts daemon-env-overrides.yaml. Before the fix the supervisor
// appended the APPLIED marker only inside `if overlayApplied`, so a no-row
// daemon got no marker; the wrapper's daemonOverlayEnv then took the no-marker
// FATAL branch on a malformed/unreadable overlay file. The fix appends the
// marker UNCONDITIONALLY for every supervised daemon, so a no-row daemon lands
// in the marker-present degrade path (empty injected key set → nil overlay
// map → manifest-only env), which returns no error.
//
// Both a global-shape task name and a serena-proxy-shape task name are
// exercised: the launch path is the SAME mergeResolvedDaemonEnvWithOverlay
// owner for both (they differ only in the task-name string, exactly as the
// supervisor descriptors differ only in Command/Args). This mirrors
// TestDaemonOverlayEnvSupervisedReloadFailureNoInjectedKeysFallsBackToNil one
// level up, at the full env-resolution entry both daemon kinds use.
func TestNoRowSupervisedDaemonSurvivesMalformedOverlay(t *testing.T) {
	tests := []struct {
		name     string
		taskName string
	}{
		{
			name:     "global-shape",
			taskName: `\mcp-local-hub-memory-default`,
		},
		{
			name:     "serena-proxy-shape",
			taskName: api.SerenaTaskNameForWorkspace(`D:\dev\no-row-workspace`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := apitest.HardenedTempDir(t)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
			// Supervisor-spawned: marker present. No overlay key set was
			// injected (this daemon had no overlay row), so the degrade
			// reconstructs an EMPTY overlay map and returns nil,nil.
			t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
			t.Setenv(daemonOverlayKeysEnvVar, "")

			// An UNRELATED operator edit left the overlay file malformed
			// (oversize → non-retryable Load error). reuse writeOversizeOverlayFile.
			writeOversizeOverlayFile(t, stateDir)

			// The resolved EnvRefs the launcher passes in (manifest-only env).
			resolved := map[string]string{
				"SERENA_DOCKER": "0",
			}
			merged, unset, err := mergeResolvedDaemonEnvWithOverlay(tt.taskName, resolved, nil)
			if err != nil {
				t.Fatalf("mergeResolvedDaemonEnvWithOverlay on no-row daemon with malformed overlay: want non-fatal (daemon would still launch), got error: %v", err)
			}
			// The manifest env survives untouched (overlay merge is a no-op).
			if got := merged["SERENA_DOCKER"]; got != "0" {
				t.Fatalf("merged SERENA_DOCKER = %q, want resolved value %q (manifest-only env on degrade)", got, "0")
			}
			if len(unset) != 0 {
				t.Fatalf("unset = %v, want empty (no omitted secrets)", unset)
			}
		})
	}
}
