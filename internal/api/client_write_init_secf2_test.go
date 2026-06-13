package api

import (
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

// SEC-F2 (security false-promise fix) regression tests.
//
// Before SEC-F2, `mcphub strict-mode enable` wrote
// supervisor-intent.json strict_mode=true and told the operator the
// strict parent-dir DACL gate was now active — but the runtime gate
// OperatorRequiresSingleUserHome() read ONLY the
// MCPHUB_REQUIRE_SINGLE_USER_HOME env var. The intent bit was inert:
// the relax lane still fired on a multi-tenant host whose operator
// believed they had enabled strict mode. These tests pin that the
// intent bit now enforces (the falsifying core is
// TestOperatorRequiresSingleUserHome_IntentStrictMode_EnvUnset), that
// the negative controls stay relaxed, that the env var still works,
// and that the intent file is read at most once per process (lazy
// cache).

// seedStrictModeIntent writes a supervisor-intent.json into the active
// (test-overridden) state dir carrying the given strict_mode bit, and
// returns the directory it wrote into. The directory is a hardened
// temp dir so the WRITE-side and READ-side parent-dir gates both pass
// on Windows without depending on %TEMP%'s ACL.
func seedStrictModeIntent(t *testing.T, strict bool) string {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version:    1,
		StrictMode: strict,
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed WriteSupervisorIntent(strict=%v): %v", strict, err)
	}
	return stateDir
}

// TestOperatorRequiresSingleUserHome_IntentStrictMode_EnvUnset is the
// FALSIFYING CORE of SEC-F2. With the env var UNSET and a persisted
// supervisor-intent.json carrying strict_mode=true,
// OperatorRequiresSingleUserHome() MUST return true.
//
// Pre-fix this returned FALSE — the intent bit was inert and only the
// env var was consulted. That false return is the exact
// security-false-promise this test guards against regressing.
func TestOperatorRequiresSingleUserHome_IntentStrictMode_EnvUnset(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")     // env unset
	t.Setenv(AllowUnhardenedStateReadEnv, "1") // read-gate inert for the temp dir
	seedStrictModeIntent(t, true)              // intent strict_mode=true
	resetStrictModeIntentCacheForTest()        // force a fresh read of the seeded intent
	t.Cleanup(resetStrictModeIntentCacheForTest)

	if !OperatorRequiresSingleUserHome() {
		t.Fatal("SEC-F2 regression: env unset + supervisor-intent.json strict_mode=true " +
			"MUST enforce strict mode, but OperatorRequiresSingleUserHome() returned false " +
			"(the intent bit is inert — this is the pre-fix false-promise)")
	}
}

// TestOperatorRequiresSingleUserHome_IntentStrictMode_False is a
// negative control: env unset + intent strict_mode=false → false. The
// intent read must not spuriously assert strict mode.
func TestOperatorRequiresSingleUserHome_IntentStrictMode_False(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	seedStrictModeIntent(t, false)
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	if OperatorRequiresSingleUserHome() {
		t.Fatal("env unset + intent strict_mode=false must NOT enforce strict mode; got true")
	}
}

// TestOperatorRequiresSingleUserHome_IntentAbsent is the
// safe-default control: env unset + NO intent file present (fresh
// install before any intent exists) → false. The read fails and the
// cache resolves to FALSE, preserving today's env-only posture.
func TestOperatorRequiresSingleUserHome_IntentAbsent(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	// Hardened temp dir but DO NOT write any supervisor-intent.json.
	t.Cleanup(SetDaemonStateRootForTest(apitest.HardenedTempDir(t)))
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	if OperatorRequiresSingleUserHome() {
		t.Fatal("env unset + absent supervisor-intent.json must default to relaxed (false); got true " +
			"(safe-default-on-error regressed)")
	}
}

// TestOperatorRequiresSingleUserHome_EnvWinsOverFalseIntent pins that
// the env var still enforces immediately even when the persisted
// intent strict_mode is false. The env half of the contract is
// unchanged by SEC-F2.
func TestOperatorRequiresSingleUserHome_EnvWinsOverFalseIntent(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1") // env explicitly strict
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	seedStrictModeIntent(t, false) // intent says relaxed
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	if !OperatorRequiresSingleUserHome() {
		t.Fatal("env MCPHUB_REQUIRE_SINGLE_USER_HOME=1 must enforce strict mode " +
			"regardless of intent strict_mode=false; got false")
	}
}

// TestOperatorRequiresSingleUserHome_IntentReadOncePerProcess proves
// the intent file is read at most ONCE per process. We can't inject a
// counting fake reader into the package helper (out of file scope, and
// the helper calls the real ReadSupervisorIntent), so laziness is
// asserted structurally: after the first OperatorRequiresSingleUserHome()
// resolves the cache, a subsequent strict_mode flip ON DISK must NOT be
// observed until the cache is reset. If the intent were re-read per
// call, the post-flip call would observe the new value; with the lazy
// cache it does not.
//
// This both proves "read once per process" and documents the
// per-process / restart-propagation semantic (a long-lived process
// keeps its first-resolved value; a restart — modeled here by the
// explicit cache reset — picks up the change).
func TestOperatorRequiresSingleUserHome_IntentReadOncePerProcess(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	stateDir := seedStrictModeIntent(t, false) // start: relaxed on disk
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// First call resolves + caches strict-from-intent = false.
	if OperatorRequiresSingleUserHome() {
		t.Fatal("precondition: intent strict_mode=false should resolve to relaxed; got true")
	}

	// Flip the on-disk intent to strict_mode=true UNDER THE SAME
	// state dir, without resetting the cache.
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	flip := &SupervisorIntentFile{Version: 1, StrictMode: true}
	if err := WriteSupervisorIntent(intentPath, flip); err != nil {
		t.Fatalf("flip WriteSupervisorIntent: %v", err)
	}

	// Without a cache reset, the lazy cache must STILL serve the
	// first-resolved value (false). If the intent were re-read per
	// call, this would observe true → proves the read is NOT per-call.
	if OperatorRequiresSingleUserHome() {
		t.Fatal("lazy-cache violation: the on-disk strict_mode flip was observed without a cache " +
			"reset; the intent must be read at most once per process (no per-call re-read)")
	}

	// Modeling a process restart: reset the cache → the next call
	// reads fresh and now observes strict_mode=true.
	resetStrictModeIntentCacheForTest()
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("after cache reset (restart model), the flipped intent strict_mode=true " +
			"must be observed; got false")
	}
}
