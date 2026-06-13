package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mcp-local-hub/internal/api"
)

// #301-3 (P2) end-to-end deadlock regression: `mcphub strict-mode disable` must
// SUCCEED in writing strict_mode=false even when (a) the parent dir is broadened
// (so the secure-write parent-dir gate would refuse) AND (b) the persisted
// intent strict_mode=true (so SEC-F2's OperatorRequiresSingleUserHome() reads
// strict from the cache). Pre-fix, the disabling intent write was self-gated by
// the OLD strict_mode=true and refused — stranding the operator in strict mode
// exactly when they ran the documented recovery path after a GPO/ACL change.
//
// Negative control: a NON-mutation secure state-file write on the SAME broadened
// parent with the SAME strict intent cache is STILL refused — the bypass is
// scoped to the strict-mode mutation's own intent/breadcrumb writes only.

// broadenedParentForGate returns a temp dir whose parent-dir DACL/mode makes the
// secure-write gate refuse (return ErrSecureWriteParentInsecure). It probes the
// real gate and SKIPs the test if the host's temp dir unexpectedly satisfies it
// (mirrors the convention in client_write_init_test.go's strict-mode tests).
//
//   - POSIX: chmod the dir to 0o777 (group/world bits trip verifyPosixParentDirFromFd).
//   - Windows: a plain t.TempDir() under %TEMP% inherits an Authenticated-Users
//     ACE the gate rejects; no chmod needed.
func broadenedParentForGate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatalf("chmod broadened dir: %v", err)
		}
	}
	// Probe the REAL write gate under explicit strict env so the relax lane
	// cannot mask a satisfied-gate host. A non-ErrSecureWriteParentInsecure
	// outcome (nil or other) means the gate did not refuse on this host.
	t.Setenv(api.RequireSingleUserHomeEnv, "1")
	probe := filepath.Join(dir, ".gate-probe.json")
	probeErr := api.WriteStateFileAtomic(probe, map[string]any{"probe": true})
	t.Setenv(api.RequireSingleUserHomeEnv, "") // restore for the test body
	_ = os.Remove(probe)
	if !errors.Is(probeErr, api.ErrSecureWriteParentInsecure) {
		t.Skipf("temp dir %s does not trip the secure-write parent gate on this host "+
			"(probe err=%v); cannot pin the broadened-parent deadlock here", dir, probeErr)
	}
	return dir
}

// TestStrictModeDisable_BroadenedParent_StrictIntent_Succeeds is the FALSIFYING
// CORE of #301-3. On a broadened parent with intent strict_mode=true, disable
// must write strict_mode=false.
func TestStrictModeDisable_BroadenedParent_StrictIntent_Succeeds(t *testing.T) {
	dir := broadenedParentForGate(t)
	// The disable flow reads supervisor-intent.json (start-of-run read + cache
	// resolution); allow the read-side relax lane so those reads succeed on the
	// broadened parent. The WRITE side is what the bypass must un-gate.
	t.Setenv(api.AllowUnhardenedStateReadEnv, "1")
	t.Setenv(api.AllowUnhardenedStateWriteEnv, "1") // allow the state-file write TOCTOU relax lane
	t.Setenv(api.RequireSingleUserHomeEnv, "")      // env NOT strict; only the intent is
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	t.Cleanup(api.ResetStrictModeIntentCacheForTest)
	t.Cleanup(api.ResetStrictModeMutationGateBypassForTest)

	intentPath := filepath.Join(dir, "supervisor-intent.json")
	// Seed intent strict_mode=true (the to-be-disabled state).
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{Version: 1, StrictMode: true}); err != nil {
		t.Fatalf("seed strict intent: %v", err)
	}
	// Force the gate cache to resolve strict=true from the seeded file, so the
	// (un-bypassed) gate WOULD refuse a write — this is the deadlock condition.
	api.ResetStrictModeIntentCacheForTest()
	if !api.OperatorRequiresSingleUserHome() {
		t.Fatal("precondition: seeded intent strict_mode=true must make the gate strict; got relaxed")
	}

	deps := StrictModeDeps{
		StateDir:         dir,
		IntentPath:       intentPath,
		BreadcrumbPath:   filepath.Join(dir, "strict-mode-mutation-incomplete.json"),
		AutostartBackend: &fakeAutostartBackend{currentStrict: true, installed: true},
		PromptOperator:   func() (string, error) { return "A", nil },
	}

	if err := RunStrictMode([]string{"disable"}, deps); err != nil {
		t.Fatalf("#301-3 regression: strict-mode disable must SUCCEED on a broadened parent "+
			"with intent strict_mode=true (the mutation-gate bypass un-gates the intent write); "+
			"got err: %v", err)
	}

	// The intent on disk must now be strict_mode=false.
	got, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read intent after disable: %v", err)
	}
	if got.StrictMode {
		t.Fatal("#301-3 regression: after a successful disable the on-disk intent strict_mode " +
			"must be false; got true (the disabling write did not land)")
	}

	// Breadcrumb must be cleaned up on the clean-success path.
	if _, statErr := os.Stat(deps.BreadcrumbPath); !os.IsNotExist(statErr) {
		t.Errorf("in-progress breadcrumb must be removed on clean disable success; stat err=%v", statErr)
	}
}

// TestStrictModeDisable_BroadenedParent_NonMutationWriteStillRefused is the
// NEGATIVE CONTROL: with the SAME broadened parent and the SAME strict intent
// cache, a NON-mutation secure state-file write is STILL refused. This proves
// the #301-3 bypass is scoped to the strict-mode mutation's own writes, not a
// process-wide relaxation that would re-open the SEC-F2 hole.
func TestStrictModeDisable_BroadenedParent_NonMutationWriteStillRefused(t *testing.T) {
	dir := broadenedParentForGate(t)
	t.Setenv(api.AllowUnhardenedStateReadEnv, "1")
	t.Setenv(api.RequireSingleUserHomeEnv, "")
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	t.Cleanup(api.ResetStrictModeIntentCacheForTest)
	t.Cleanup(api.ResetStrictModeMutationGateBypassForTest)

	intentPath := filepath.Join(dir, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{Version: 1, StrictMode: true}); err != nil {
		t.Fatalf("seed strict intent: %v", err)
	}
	api.ResetStrictModeIntentCacheForTest()
	if !api.OperatorRequiresSingleUserHome() {
		t.Fatal("precondition: seeded intent strict_mode=true must make the gate strict; got relaxed")
	}

	// A NON-mutation state-file write to an UNRELATED path, with NO bypass
	// window open, must be refused by the strict (intent-derived) gate.
	unrelated := filepath.Join(dir, "unrelated-state.json")
	err := api.WriteStateFileAtomic(unrelated, map[string]any{"not": "a strict-mode mutation"})
	if !errors.Is(err, api.ErrSecureWriteParentInsecure) {
		t.Fatalf("negative control: a non-mutation secure write on a broadened parent with intent "+
			"strict_mode=true MUST be refused with ErrSecureWriteParentInsecure (the bypass must NOT "+
			"leak beyond the strict-mode mutation writes); got err=%v", err)
	}
	if _, statErr := os.Stat(unrelated); !os.IsNotExist(statErr) {
		t.Errorf("refused non-mutation write must not leave a file; stat err=%v", statErr)
	}
}
