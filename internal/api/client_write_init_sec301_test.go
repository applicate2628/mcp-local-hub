package api

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

// #301-2 (P2) + #301-3 (P2) regression tests.
//
// #301-2: readStrictModeFromIntentBestEffort must distinguish an ABSENT intent
// (fresh install → relax) from a READ FAILURE on an EXISTING intent
// (decode/permission/parent-gate refusal → fail-closed-to-STRICT). Before the
// fix, ANY read error relaxed, silently disabling the strict gate whenever the
// gate-controlling file became unreadable.
//
// #301-3: the strict-mode mutation write of supervisor-intent.json must use the
// ENV-ONLY gate (skip the cached intent.strict_mode) for the duration of the
// write, so the OLD strict_mode=true cannot self-gate the write of the NEW
// (disabling) value on a broadened parent.

// ---------------------------------------------------------------------------
// #301-2: read-error → fail-closed-to-strict
// ---------------------------------------------------------------------------

// TestReadStrictModeFromIntent_DecodeError_FailsClosedToStrict is the
// FALSIFYING CORE of #301-2. An EXISTING supervisor-intent.json that cannot be
// decoded (corrupt/truncated/attacker-clobbered) must resolve to strict=TRUE,
// not relax. Pre-fix any read error returned false (relax), silently disabling
// the gate.
func TestReadStrictModeFromIntent_DecodeError_FailsClosedToStrict(t *testing.T) {
	t.Setenv(AllowUnhardenedStateReadEnv, "1") // read-gate inert for the temp dir
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	// Write a NON-JSON body to the intent path: the file EXISTS but
	// ReadSupervisorIntent will fail at json.Unmarshal (a decode error, NOT
	// os.ErrNotExist).
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := os.WriteFile(intentPath, []byte("this is not json {{{"), 0o600); err != nil {
		t.Fatalf("write malformed intent: %v", err)
	}

	if got := readStrictModeFromIntentBestEffort(); !got {
		t.Fatal("#301-2 regression: a decode error on an EXISTING supervisor-intent.json " +
			"must fail CLOSED to strict (return true); got false (the pre-fix fail-open-to-relax " +
			"that silently disables the gate when the gate-controlling file is unreadable)")
	}
}

// TestReadStrictModeFromIntent_Absent_Relaxes is the safe-default control:
// a MISSING intent file (fresh install, never ran strict-mode enable) resolves
// to relax (false). The absent path must stay env-only, unchanged by #301-2.
func TestReadStrictModeFromIntent_Absent_Relaxes(t *testing.T) {
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	// Deliberately write NO supervisor-intent.json.

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("absent supervisor-intent.json (fresh install) must relax (return false); got true " +
			"(#301-2 must not flip the ABSENT case to strict)")
	}
}

// TestReadStrictModeFromIntent_PresentValues confirms a well-formed intent
// still returns its strict_mode bit verbatim (true→true, false→false) — the
// read-error refinement must not perturb the success path.
func TestReadStrictModeFromIntent_PresentValues(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run(map[bool]string{true: "strict", false: "relaxed"}[want], func(t *testing.T) {
			t.Setenv(AllowUnhardenedStateReadEnv, "1")
			stateDir := apitest.HardenedTempDir(t)
			t.Cleanup(SetDaemonStateRootForTest(stateDir))
			intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
			if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, StrictMode: want}); err != nil {
				t.Fatalf("seed intent strict=%v: %v", want, err)
			}
			if got := readStrictModeFromIntentBestEffort(); got != want {
				t.Fatalf("readStrictModeFromIntentBestEffort()=%v, want %v (verbatim strict_mode bit)", got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// #301-3: mutation-gate bypass at the gate-decision level
// ---------------------------------------------------------------------------

// TestStrictModeMutationGateBypass_EnvOnlyDuringWindow is the FALSIFYING CORE of
// #301-3 at the decision boundary. With the env var UNSET and the intent cache
// resolved to strict=true, OperatorRequiresSingleUserHome() returns true
// NORMALLY (the gate the disabling write must pass), but returns FALSE while a
// mutation-gate bypass window is open — proving the write of the NEW intent is
// governed by env only, breaking the self-gating deadlock. After the window
// closes, the gate is strict again.
func TestStrictModeMutationGateBypass_EnvOnlyDuringWindow(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")     // env unset → intent governs normally
	t.Setenv(AllowUnhardenedStateReadEnv, "1") // read-gate inert for the temp dir

	// Seed a persisted strict=true intent and force the cache to resolve it.
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, StrictMode: true}); err != nil {
		t.Fatalf("seed strict intent: %v", err)
	}
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)
	t.Cleanup(resetStrictModeMutationGateBypassForTest)

	// Normal: intent strict=true governs.
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("precondition: env unset + intent strict_mode=true must enforce strict; got false")
	}

	// Inside the bypass window: env-only → env is unset → relaxed.
	err := WithStrictModeMutationGateBypass(func() error {
		if OperatorRequiresSingleUserHome() {
			t.Error("#301-3 regression: inside the mutation-gate bypass, the gate must consult " +
				"the env var ONLY (env unset → false), so the disabling intent write is not " +
				"self-gated by the stale strict_mode=true; got true (deadlock not broken)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithStrictModeMutationGateBypass returned err: %v", err)
	}

	// After the window: strict again (scope discipline — the bypass is the
	// write window only, not the process lifetime).
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("after the bypass window closed, the intent strict_mode=true must govern again; " +
			"got false (the bypass leaked beyond its window)")
	}
}

// TestStrictModeMutationGateBypass_EnvStillWinsInsideWindow pins that the bypass
// only DROPS the intent-cache input — it does NOT relax an operator who set the
// env var explicitly. With env=1, the gate stays strict even inside the window.
// (The mutation write of a broadened-parent host under an explicit env strict
// posture is correctly NOT exempted — only the intent-derived strict is.)
func TestStrictModeMutationGateBypass_EnvStillWinsInsideWindow(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1") // explicit env strict
	t.Cleanup(resetStrictModeMutationGateBypassForTest)

	if err := WithStrictModeMutationGateBypass(func() error {
		if !OperatorRequiresSingleUserHome() {
			t.Error("env MCPHUB_REQUIRE_SINGLE_USER_HOME=1 must still enforce strict even inside " +
				"the mutation-gate bypass; the bypass drops the intent-cache input only, not the env")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithStrictModeMutationGateBypass returned err: %v", err)
	}
}

// TestStrictModeMutationGateBypass_NestedReentrant proves nested Begin/End pairs
// keep the bypass active until the OUTERMOST End (depth-counted), so a helper
// that opens its own window inside an already-open window cannot prematurely
// re-enable the intent gate mid-mutation.
func TestStrictModeMutationGateBypass_NestedReentrant(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, StrictMode: true}); err != nil {
		t.Fatalf("seed strict intent: %v", err)
	}
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)
	t.Cleanup(resetStrictModeMutationGateBypassForTest)

	BeginStrictModeMutationGateBypass()
	BeginStrictModeMutationGateBypass()
	if OperatorRequiresSingleUserHome() {
		t.Fatal("depth-2 bypass: gate must be env-only (false); got true")
	}
	EndStrictModeMutationGateBypass()
	// Still depth 1 → still bypassed.
	if OperatorRequiresSingleUserHome() {
		t.Fatal("depth-1 bypass after one End: gate must still be env-only (false); got true")
	}
	EndStrictModeMutationGateBypass()
	// Back to depth 0 → intent strict governs.
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("depth-0 after outermost End: intent strict_mode=true must govern; got false")
	}
	// Defensive double-End must not drive the counter negative. After an
	// over-End the depth stays clamped at 0, so the gate stays strict AND a
	// later single Begin still bypasses correctly (no negative-depth leak).
	EndStrictModeMutationGateBypass()
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("after an over-End the gate must still be strict (counter clamps at 0, not negative); got relaxed")
	}
	// Prove the clamp left a clean depth of 0: a single Begin must bypass, and
	// its End must restore strict — not require two Ends (which a negative
	// counter would have demanded).
	BeginStrictModeMutationGateBypass()
	if OperatorRequiresSingleUserHome() {
		t.Fatal("single Begin after the over-End must bypass (env-only → false); got strict " +
			"(the over-End drove the counter negative)")
	}
	EndStrictModeMutationGateBypass()
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("single End after the single Begin must restore strict; got relaxed")
	}
}
