package api

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// TestReadStrictModeFromIntent_DecodeError_Relaxes pins the pr301 r10
// behavior: an EXISTING but UNPARSEABLE supervisor-intent.json
// (corrupt/truncated/attacker-clobbered) resolves to relax (false), not strict.
// Intent-file strict is BEST-EFFORT and read gate-free — a body that cannot be
// parsed declares no parseable strict_mode bit, so the env var is the robust
// strict mitigation. This RE-POINTS the prior #301-2 decode→strict assertion
// (which read through ReadSupervisorIntent's parent gate) to the gate-free
// relax-on-parse-error verdict.
func TestReadStrictModeFromIntent_DecodeError_Relaxes(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	// Write a NON-JSON body to the intent path: the file EXISTS but the
	// gate-free json.Unmarshal fails (a parse error, NOT os.ErrNotExist).
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := os.WriteFile(intentPath, []byte("this is not json {{{"), 0o600); err != nil {
		t.Fatalf("write malformed intent: %v", err)
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r10: a parse error on an EXISTING supervisor-intent.json must RELAX " +
			"(return false) — intent-file strict is best-effort, read gate-free; an unparseable " +
			"body declares no strict_mode bit, and the robust strict path is " +
			"MCPHUB_REQUIRE_SINGLE_USER_HOME=1; got true")
	}
}

// TestReadStrictModeFromIntent_PathUnresolvable_Relaxes pins the pr301 r10
// behavior: when the state dir cannot be RESOLVED at all (DaemonStateDirReadOnly
// returns an error), the intent read relaxes (returns false). Intent-file strict
// is BEST-EFFORT — with no resolvable path the bit cannot be read, and the
// robust strict path is the env var (MCPHUB_REQUIRE_SINGLE_USER_HOME=1, checked
// in OperatorRequiresSingleUserHome BEFORE this read). This RE-POINTS the prior
// pr301 r4 unresolvable→strict assertion to the best-effort-relax verdict.
//
// Engineering a GENUINE DaemonStateDirReadOnly error cross-platform: the
// daemonStateRootOverride seam short-circuits the resolver, so it CANNOT be used
// to force a resolve error. Instead, clear the override and make the real
// resolver fail:
//
//   - Windows (test_state_path_env build): stub knownFolderResolverFn to error
//     AND clear LOCALAPPDATA + USERPROFILE so resolveKnownFolderWithEnvFallback
//     exhausts every fallback and returns errKnownFolderUnavailable.
//   - POSIX: clear XDG_DATA_HOME + HOME so posixParentDir's os.UserHomeDir fails.
//
// statePathsHelper saves/restores the override + resolver (panic-safe).
func TestReadStrictModeFromIntent_PathUnresolvable_Relaxes(t *testing.T) {
	// Clear the override (statePathsHelper restores it) so the REAL resolver runs.
	statePathsHelper(t)
	daemonStateRootOverride = ""

	if runtime.GOOS == "windows" {
		// Force the KnownFolder resolver and BOTH env fallbacks to fail.
		installKnownFolderStub(t, func() (string, error) {
			return "", errors.New("stub: KnownFolder unavailable")
		})
		t.Setenv("LOCALAPPDATA", "")
		t.Setenv("USERPROFILE", "")
	} else {
		// Force posixParentDir's os.UserHomeDir to fail.
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "")
	}
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Precondition: the resolver must genuinely error (so this test exercises the
	// path-resolution-error branch, not some other relax/strict path).
	if dir, err := DaemonStateDirReadOnly(); err == nil {
		t.Fatalf("precondition: DaemonStateDirReadOnly() must return an error when the resolver "+
			"and all fallbacks fail; got dir=%q nil err (the unresolvable-path branch under test "+
			"is not being exercised)", dir)
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r10: an UNRESOLVABLE supervisor-intent path must RELAX (return false) — " +
			"intent-file strict is best-effort and cannot be read with no resolvable path; the " +
			"robust strict path is MCPHUB_REQUIRE_SINGLE_USER_HOME=1 (checked before this read); " +
			"got true (the reverted pre-r10 unresolvable→strict over-reach)")
	}
}

// TestReadStrictModeFromIntent_Absent_Relaxes is the safe-default control:
// a MISSING intent file (fresh install, never ran strict-mode enable) resolves
// to relax (false). pr301 r9 reverted the r5/r6/r7 absent-on-delete-capable →
// strict over-reach, so an absent intent relaxes regardless of the dir's
// broadening (delete-capable absent-relax is covered by the sec301
// windows/posix tests; here the dir is hardened, the simplest relax case).
func TestReadStrictModeFromIntent_Absent_Relaxes(t *testing.T) {
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	// Deliberately write NO supervisor-intent.json.

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("absent supervisor-intent.json (fresh install) must relax (return false); got true " +
			"(the absent case must not flip to strict — pr301 r9 revert)")
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
