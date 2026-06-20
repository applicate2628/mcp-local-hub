package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// seedStrictIntent writes a strict_mode=true supervisor-intent.json through the
// hardened state-file writer so the fixture remains readable by the
// inode-anchored reader.
func seedStrictIntent(t *testing.T, intentPath string) {
	t.Helper()
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{Version: 1, StrictMode: true}); err != nil {
		t.Fatalf("seed strict intent: %v", err)
	}
}

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
	seedStrictIntent(t, intentPath)
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
	api.ResetStrictModeIntentCacheForTest()
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

// TestStrictModeDisable_BroadenedParent_NonMutationWritePersistedStrictRejects
// is the strict-scope control: with the same broadened parent and persisted
// strict intent cache, a non-mutation secure state-file write is refused even
// when MCPHUB_REQUIRE_SINGLE_USER_HOME is unset. Only the strict-mode
// mutation write itself gets the bounded bypass.
//
// SELF-HEAL HAZARD (pr301 r3 Finding 1): the negative-control write target's
// PARENT must be a directory that DaemonStateDir()'s ensureStateRoot chmod
// can NEVER reach. On POSIX, resolving the strict-mode cache routes through
// DefaultSupervisorIntentPath → DaemonStateDir → ensureStateRoot(<state-root>),
// whose POSIX leg runs os.Chmod(<state-root>, 0o700). If the write target sat
// DIRECTLY under the state root (its parent == the state root), that chmod
// would heal the broadened bits back to 0700 BEFORE the write gate ran — the
// gate would then pass, the write would succeed, and the negative control
// would FALSELY fail on Linux while no longer exercising the broadened-parent
// scenario at all. We therefore point the write at a path inside a CHILD
// subdir of the state root and broaden that CHILD: ensureStateRoot chmods only
// the root, never its children, so the child stays broadened at write time.
// (On Windows ensureStateRoot is MkdirAll-only — it never touches DACLs — so
// the root would survive too, but the child structure keeps both platforms on
// one code path.)
func TestStrictModeDisable_BroadenedParent_NonMutationWritePersistedStrictRejects(t *testing.T) {
	dir := broadenedParentForGate(t)
	t.Setenv(api.AllowUnhardenedStateReadEnv, "1")
	t.Setenv(api.RequireSingleUserHomeEnv, "")
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	t.Cleanup(api.ResetStrictModeIntentCacheForTest)
	t.Cleanup(api.ResetStrictModeMutationGateBypassForTest)

	intentPath := filepath.Join(dir, "supervisor-intent.json")
	seedStrictIntent(t, intentPath)
	api.ResetStrictModeIntentCacheForTest()
	if !api.OperatorRequiresSingleUserHome() {
		t.Fatal("precondition: seeded intent strict_mode=true must make the gate strict; got relaxed")
	}

	// Build a CHILD subdir under the state root and broaden IT (not the root).
	// ensureStateRoot only ever chmods the state root, so a broadened child
	// survives every DaemonStateDir() resolution that the gate triggers.
	childParent := filepath.Join(dir, "broadened-child")
	if err := os.MkdirAll(childParent, 0o700); err != nil {
		t.Fatalf("mkdir broadened-child: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(childParent, 0o777); err != nil {
			t.Fatalf("chmod broadened-child: %v", err)
		}
	}

	// PROVE the negative control genuinely faces a broadened parent at write
	// time: re-resolve the strict cache (the exact chain whose ensureStateRoot
	// chmod the self-heal hazard rides on) and then assert the child parent is
	// STILL broadened. On POSIX we check the mode bits directly; on Windows the
	// child inherits the broadened temp-dir DACL that the write gate rejects,
	// which the actual write-refusal assertion below verifies.
	api.ResetStrictModeIntentCacheForTest()
	_ = api.OperatorRequiresSingleUserHome() // drives ensureStateRoot(<root>) chmod
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(childParent)
		if statErr != nil {
			t.Fatalf("stat broadened-child after cache resolution: %v", statErr)
		}
		if info.Mode().Perm()&0o022 == 0 {
			t.Fatalf("self-heal hazard: the broadened child parent (mode %#o) was healed before the "+
				"negative-control write — the test would no longer exercise a broadened parent",
				info.Mode().Perm())
		}
	}

	// A non-mutation state-file write inside the broadened child parent, with no
	// bypass window open, must still honor the persisted strict intent.
	unrelated := filepath.Join(childParent, "unrelated-state.json")
	err := api.WriteStateFileAtomic(unrelated, map[string]any{"not": "a strict-mode mutation"})
	if err == nil {
		t.Fatalf("non-mutation write on broadened parent must reject while persisted strict_mode=true is cached")
	}
	if !strings.Contains(err.Error(), "persisted supervisor-intent.json") {
		t.Errorf("error must mention persisted strict-mode intent; got %v", err)
	}
	if _, statErr := os.Stat(unrelated); !os.IsNotExist(statErr) {
		t.Errorf("persisted strict-mode rejection leaked the non-mutation target (stat err=%v)", statErr)
	}
}
