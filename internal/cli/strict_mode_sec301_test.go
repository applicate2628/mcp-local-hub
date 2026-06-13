package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mcp-local-hub/internal/api"
)

// seedStrictIntentRaw writes a strict_mode=true supervisor-intent.json directly
// via os.WriteFile, bypassing the secure-write operator gate.
//
// Seeding is fixture setup, not the unit under test, so a raw write is the
// correct gate-free path: it deterministically lands the strict=true intent on
// the broadened parent without depending on the secure-write relax lane (or on
// any intent-cache resolution order). The test then resets the cache so the
// next resolution reads the now-PRESENT strict=true intent — exactly the
// #301-3 deadlock condition these tests exercise (a stale present strict_mode
// must not self-gate the disabling write).
//
// (Historical note: pr301 r5/r6/r7 made an ABSENT intent on a delete-capable
// broadened dir resolve strict, so a gated seed write self-refused — the
// chicken-and-egg the raw seed sidestepped. pr301 r9 reverted absent → relax,
// so that self-refusal no longer occurs; the raw seed is retained as the
// simplest deterministic fixture path.)
func seedStrictIntentRaw(t *testing.T, intentPath string) {
	t.Helper()
	raw, err := json.Marshal(&api.SupervisorIntentFile{Version: 1, StrictMode: true})
	if err != nil {
		t.Fatalf("marshal seed intent: %v", err)
	}
	if err := os.WriteFile(intentPath, raw, 0o600); err != nil {
		t.Fatalf("seed strict intent (raw): %v", err)
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
	// Seed intent strict_mode=true (the to-be-disabled state) via a gate-free raw
	// write — a gated WriteSupervisorIntent would now self-refuse on this
	// broadened parent (pr301 r5 Finding 1; see seedStrictIntentRaw).
	seedStrictIntentRaw(t, intentPath)
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
func TestStrictModeDisable_BroadenedParent_NonMutationWriteStillRefused(t *testing.T) {
	dir := broadenedParentForGate(t)
	t.Setenv(api.AllowUnhardenedStateReadEnv, "1")
	t.Setenv(api.RequireSingleUserHomeEnv, "")
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	t.Cleanup(api.ResetStrictModeIntentCacheForTest)
	t.Cleanup(api.ResetStrictModeMutationGateBypassForTest)

	intentPath := filepath.Join(dir, "supervisor-intent.json")
	// Gate-free raw seed (pr301 r5 Finding 1; see seedStrictIntentRaw): a gated
	// write would self-refuse on this broadened parent.
	seedStrictIntentRaw(t, intentPath)
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

	// A NON-mutation state-file write INSIDE the broadened CHILD parent, with
	// NO bypass window open, must be refused by the strict (intent-derived)
	// gate. The child parent's broadened bits/ACE are what the gate rejects.
	unrelated := filepath.Join(childParent, "unrelated-state.json")
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
