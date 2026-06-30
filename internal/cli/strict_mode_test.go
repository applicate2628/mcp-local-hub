// Tests for `mcphub strict-mode {enable, disable, --recover}` — Task 11.2
// (plan §2543-2603). The strict-mode CLI must execute the two-resource
// mutation (supervisor-intent.json + autostart shim) atomically: revert
// step 1 on step 2 failure, and write a breadcrumb on revert failure so
// `--recover` can later reconcile manually.
//
// All tests use an in-memory fake autostart backend (no real Task
// Scheduler / systemd / launchctl) and a temp state dir (no real
// migration.lock collision). The fake exposes two failure switches —
// shimFail (step 2 fail) and revertFail (step 1 revert fail) — which
// the tests flip to drive each branch of the revert ladder.
package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/autostart"
)

// fakeAutostartBackend is the in-memory test seam injected through
// StrictModeDeps.AutostartBackend. It records the latest Enable opts
// for assertion + supports two failure switches.
type fakeAutostartBackend struct {
	enableCalls   []autostart.Options
	disableCalls  int
	failEnable    bool         // when true, Enable returns shimErr
	shimErr       error        // err returned on shimFail path
	currentStrict bool         // last successful StrictMode the shim was set to
	installed     bool         // last successful Enable left the shim installed
	onEnable      func() error // optional hook fired at the START of Enable (step 2)

	// statusFn, when non-nil, fully overrides Status. It lets a test model a
	// per-OS backend that mutated the shim BEFORE its Enable error returned —
	// i.e. the snapshot taken before step 2 and the re-probe taken after the
	// failed Enable can disagree (the torn-shim signal). When nil, Status
	// falls back to the legacy installed/StateEnabledRunning behavior so every
	// pre-existing test keeps its semantics.
	statusFn     func(opts autostart.Options) (autostart.State, error)
	statusSpecFn func(opts autostart.Options) string
}

func (f *fakeAutostartBackend) Enable(opts autostart.Options) error {
	// onEnable fires at the moment step 2 begins — i.e. AFTER step 1 (intent
	// write) and the in-progress breadcrumb write, BEFORE the shim flips. FIX 3
	// tests observe the breadcrumb-on-disk state here to prove the in-progress
	// marker is written in the step1→step2 window.
	if f.onEnable != nil {
		if err := f.onEnable(); err != nil {
			return err
		}
	}
	if f.failEnable {
		return f.shimErr
	}
	f.enableCalls = append(f.enableCalls, opts)
	f.currentStrict = opts.StrictMode
	f.installed = true
	return nil
}

func (f *fakeAutostartBackend) Disable() error {
	f.disableCalls++
	f.installed = false
	return nil
}

func (f *fakeAutostartBackend) Status(opts autostart.Options) (autostart.State, error) {
	if f.statusFn != nil {
		return f.statusFn(opts)
	}
	if !f.installed {
		return autostart.StateAbsent, nil
	}
	return autostart.StateEnabledRunning, nil
}

func (f *fakeAutostartBackend) StatusSnapshot(opts autostart.Options) (autostart.StatusSnapshot, error) {
	state, err := f.Status(opts)
	spec := ""
	if f.statusSpecFn != nil {
		spec = f.statusSpecFn(opts)
	}
	return autostart.StatusSnapshot{State: state, SpecFingerprint: spec}, err
}

// strictModeFixture is the test harness referenced in plan §2553-2600.
type strictModeFixture struct {
	t              *testing.T
	stateDir       string
	intentPath     string
	breadcrumbPath string
	backend        *fakeAutostartBackend

	// revertFail is the second failure switch: when true, the revert
	// write (re-write of supervisor-intent.json after shimFail) ALSO
	// fails. Implemented by replacing the intent path with a directory
	// at revert time (rename will fail).
	revertFail bool
}

func setupSupervisorFixture(t *testing.T) *strictModeFixture {
	t.Helper()
	// v0.5.0 Fix Group 5: WriteSupervisorIntent flows through the
	// hardened secure-write pipeline (handle-bound DACL, parent-dir
	// gate, post-rename re-verify). Test temp dirs must pass the
	// parent-dir gate, which t.TempDir() alone does not on machines
	// whose %TEMP%/TMPDIR has Authenticated Users (or equivalent)
	// write rights. apitest.HardenedTempDir installs the allowlist-
	// conforming DACL/mode the gate expects.
	dir := apitest.HardenedTempDir(t)
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	// Seed initial intent with StrictMode=false so revert has a value
	// to drive back to.
	initial := &api.SupervisorIntentFile{
		Version:    1,
		UpdatedAt:  "1970-01-01T00:00:00Z",
		StrictMode: false,
	}
	if err := api.WriteSupervisorIntent(intentPath, initial); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	return &strictModeFixture{
		t:              t,
		stateDir:       dir,
		intentPath:     intentPath,
		breadcrumbPath: filepath.Join(dir, "strict-mode-mutation-incomplete.json"),
		backend:        &fakeAutostartBackend{},
	}
}

// SeedInitialStrict overrides the seeded StrictMode (default false).
// Used by disable-path tests where the starting intent must be true.
func (f *strictModeFixture) SeedInitialStrict(v bool) {
	f.t.Helper()
	intent := &api.SupervisorIntentFile{
		Version:    1,
		UpdatedAt:  "1970-01-01T00:00:00Z",
		StrictMode: v,
	}
	if err := api.WriteSupervisorIntent(f.intentPath, intent); err != nil {
		f.t.Fatalf("re-seed intent: %v", err)
	}
	f.backend.currentStrict = v
	f.backend.installed = true
}

// MakeShimWriteFail flips the fake autostart backend to fail on Enable.
func (f *strictModeFixture) MakeShimWriteFail() {
	f.backend.failEnable = true
	f.backend.shimErr = errors.New("simulated shim write failure")
}

// MakeRevertWriteFail makes the revert step (re-write of
// supervisor-intent.json) fail by replacing the parent dir of the
// intent path with a file. The first write succeeded (atomic rename
// onto the seeded file), but the *revert* re-issues WriteSupervisorIntent
// which goes through CreateTemp in the same dir; replacing the dir
// with a non-dir guarantees the temp create fails.
//
// Test seam implementation: the production code calls
// api.MutateSupervisorIntentIfChanged through the deps.MutateIntentFn
// function pointer; revertFail flips that pointer to a stub that returns an
// error specifically on the revert call. We use a counter to distinguish
// step-1 write (succeeds) from revert write (fails).
func (f *strictModeFixture) MakeRevertWriteFail() {
	f.revertFail = true
}

// Deps assembles the StrictModeDeps the unit-under-test consumes. The
// MutateIntentFn override is the canonical seam — production deps leave
// it nil so RunStrictMode falls back to api.MutateSupervisorIntentIfChanged.
func (f *strictModeFixture) Deps() StrictModeDeps {
	d := StrictModeDeps{
		StateDir:         f.stateDir,
		IntentPath:       f.intentPath,
		BreadcrumbPath:   f.breadcrumbPath,
		AutostartBackend: f.backend,
		PromptOperator:   func() (string, error) { return "A", nil },
	}
	if f.revertFail {
		writes := 0
		d.MutateIntentFn = func(path string, mutate func(*api.SupervisorIntentFile) (bool, error)) error {
			return api.MutateSupervisorIntentIfChanged(path, func(file *api.SupervisorIntentFile) (bool, error) {
				changed, err := mutate(file)
				if err != nil || !changed {
					return changed, err
				}
				writes++
				if writes == 1 {
					// First write (step 1) succeeds.
					return true, nil
				}
				// Subsequent writes (the revert) fail without publishing.
				return false, errors.New("simulated revert write failure")
			})
		}
	}
	return d
}

func (f *strictModeFixture) IntentPath() string     { return f.intentPath }
func (f *strictModeFixture) BreadcrumbPath() string { return f.breadcrumbPath }

// ReadShimArgs returns a synthetic string representation of the last
// recorded shim Enable opts. Tests grep for "--strict-mode" to check
// the StrictMode flag wiring (the real shim's argv is a per-OS
// concern; the contract here is just "shim got told strict-mode=true").
func (f *strictModeFixture) ReadShimArgs() string {
	if len(f.backend.enableCalls) == 0 {
		return ""
	}
	last := f.backend.enableCalls[len(f.backend.enableCalls)-1]
	if last.StrictMode {
		return "mcphub supervise --strict-mode"
	}
	return "mcphub supervise"
}

// ReadBreadcrumb parses the on-disk breadcrumb JSON.
func (f *strictModeFixture) ReadBreadcrumb() *strictModeBreadcrumb {
	f.t.Helper()
	raw, err := os.ReadFile(f.breadcrumbPath)
	if err != nil {
		f.t.Fatalf("read breadcrumb: %v", err)
	}
	var bc strictModeBreadcrumb
	if err := json.Unmarshal(raw, &bc); err != nil {
		f.t.Fatalf("unmarshal breadcrumb: %v", err)
	}
	return &bc
}

// exitCodeFromError extracts the integer exit code from a returned
// strictModeExitError; returns 0 when nil, -1 when non-nil but not an
// exit-coded error so tests fail loudly.
func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var fe interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	if errors.As(err, &fe) {
		return fe.ExitCode()
	}
	return -1
}

// ============================================================================
// Plan §2553 — happy-path two-resource atomic mutation.
// ============================================================================

func TestStrictModeEnable_AtomicTwoResource(t *testing.T) {
	tmp := setupSupervisorFixture(t)

	if err := RunStrictMode([]string{"enable"}, tmp.Deps()); err != nil {
		t.Fatalf("enable: %v", err)
	}
	intent, err := api.ReadSupervisorIntent(tmp.IntentPath())
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if !intent.StrictMode {
		t.Fatal("intent.strict_mode not set")
	}
	shimArgs := tmp.ReadShimArgs()
	if !strings.Contains(shimArgs, "--strict-mode") {
		t.Fatalf("shim args missing --strict-mode: got %q", shimArgs)
	}
}

// ============================================================================
// Plan §2569 — revert-on-shim-failure leaves intent at original value.
// ============================================================================

func TestStrictModeEnable_RevertOnShimFailure(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.MakeShimWriteFail()

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if err == nil {
		t.Fatal("expected non-zero exit when shim write fails")
	}
	intent, readErr := api.ReadSupervisorIntent(tmp.IntentPath())
	if readErr != nil {
		t.Fatalf("read intent: %v", readErr)
	}
	if intent.StrictMode {
		t.Fatal("intent.strict_mode not reverted after shim failure")
	}
	// Breadcrumb must NOT exist on the happy revert path (only when
	// revert ITSELF fails).
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("breadcrumb written on happy revert path (should only exist on revert failure)")
	}
}

// ============================================================================
// Plan §2584 — revert-of-revert failure writes the breadcrumb + exits 10.
// ============================================================================

func TestStrictModeEnable_RevertOfRevertFailure_BreadcrumbWritten(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.MakeShimWriteFail()
	tmp.MakeRevertWriteFail()

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeRevertFailed {
		t.Fatalf("expected exit %d (STRICT_MODE_REVERT_FAILED), got %d (err=%v)", ExitStrictModeRevertFailed, exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
		t.Fatalf("breadcrumb missing: %v", statErr)
	}
	bc := tmp.ReadBreadcrumb()
	if bc.Intended != true {
		t.Errorf("breadcrumb.intended = %v, want true", bc.Intended)
	}
	// actual_intent_state must reflect the state of intent AFTER step-1
	// succeeded but BEFORE revert tried — which is true (step 1 flipped
	// false → true).
	if bc.ActualIntentState != true {
		t.Errorf("breadcrumb.actual_intent_state = %v, want true", bc.ActualIntentState)
	}
	// actual_shim_state stays at false because step-2 failed.
	if bc.ActualShimState != false {
		t.Errorf("breadcrumb.actual_shim_state = %v, want false", bc.ActualShimState)
	}
	if bc.RevertError == "" {
		t.Error("breadcrumb missing revert_error")
	}
	if bc.Step2Error == "" {
		t.Error("breadcrumb missing step2_error")
	}
	if bc.TS == "" {
		t.Error("breadcrumb missing ts")
	}
}

// ============================================================================
// FIX 3 (opus-3 F11) — forward-progress breadcrumb for the SIGKILL/power-loss
// window between step 1 (intent write) and step 2 (shim Enable).
// ============================================================================

// TestStrictModeEnable_InProgressBreadcrumbWrittenBeforeStep2 is the FIX 3
// falsifying regression. The in-progress breadcrumb must exist on disk at the
// moment step 2 (shim Enable) begins — i.e. it was written in the step1→step2
// window. If the process is SIGKILLed there, this marker is the only thing that
// lets `mcphub strict-mode --recover` detect the posture drift.
//
// Pre-fix NO breadcrumb was written before step 1, so the marker is absent at
// step 2 and the test fails.
func TestStrictModeEnable_InProgressBreadcrumbWrittenBeforeStep2(t *testing.T) {
	tmp := setupSupervisorFixture(t)

	var bcAtStep2 *strictModeBreadcrumb
	var intentStrictAtStep2 bool
	tmp.backend.onEnable = func() error {
		// Read the breadcrumb that should have been written BEFORE step 1.
		raw, err := os.ReadFile(tmp.BreadcrumbPath())
		if err != nil {
			return nil // leave bcAtStep2 nil — the assertion below fails loudly
		}
		var bc strictModeBreadcrumb
		if err := json.Unmarshal(raw, &bc); err != nil {
			t.Errorf("breadcrumb at step 2 is not valid JSON: %v", err)
			return nil
		}
		bcAtStep2 = &bc
		// Also confirm step 1 already flipped intent to the desired value, so
		// this really is the step1→step2 window.
		if intent, ierr := api.ReadSupervisorIntent(tmp.IntentPath()); ierr == nil {
			intentStrictAtStep2 = intent.StrictMode
		}
		return nil
	}

	if err := RunStrictMode([]string{"enable"}, tmp.Deps()); err != nil {
		t.Fatalf("enable: %v", err)
	}

	if bcAtStep2 == nil {
		t.Fatal("no in-progress breadcrumb on disk when step 2 began — FIX 3 requires it written before step 1 (opus-3 F11)")
	}
	if bcAtStep2.Phase != strictModeBreadcrumbPhaseInProgress {
		t.Errorf("breadcrumb.phase = %q at step 2, want %q", bcAtStep2.Phase, strictModeBreadcrumbPhaseInProgress)
	}
	if !bcAtStep2.Intended {
		t.Errorf("breadcrumb.intended = false at step 2, want true (enable)")
	}
	// ActualIntentState records the PRE-mutation value (false) so --recover
	// branch B rolls back to it.
	if bcAtStep2.ActualIntentState {
		t.Errorf("breadcrumb.actual_intent_state = true at step 2, want false (pre-mutation value for rollback)")
	}
	if !intentStrictAtStep2 {
		t.Errorf("intent was not flipped to desired before step 2 — window assumption broken")
	}
}

// TestStrictModeEnable_InProgressBreadcrumbDeletedAfterSuccess pins the
// clean-success tail: once both resources reach `desired`, the in-progress
// breadcrumb is deleted so the next strict-mode invocation does not refuse on a
// stale marker.
func TestStrictModeEnable_InProgressBreadcrumbDeletedAfterSuccess(t *testing.T) {
	tmp := setupSupervisorFixture(t)

	if err := RunStrictMode([]string{"enable"}, tmp.Deps()); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := os.Stat(tmp.BreadcrumbPath()); err == nil {
		t.Fatal("in-progress breadcrumb survived a fully successful mutation — clean-success tail must delete it (FIX 3)")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}

// TestStrictModeEnable_InProgressBreadcrumbDeletedAfterRevert pins that the
// revert-success path (step 2 fails, revert of step 1 succeeds) also deletes
// the in-progress breadcrumb — there is no drift once both resources are back
// at their original value, so a stale marker would force a spurious --recover.
// This is the existing happy-revert contract (TestStrictModeEnable_RevertOnShimFailure
// asserts the breadcrumb is absent) restated for the in-progress marker.
func TestStrictModeEnable_InProgressBreadcrumbDeletedAfterRevert(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.MakeShimWriteFail()

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if err == nil {
		t.Fatal("expected non-zero exit when shim write fails")
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("in-progress breadcrumb survived a successful revert — revert-success path must delete it (FIX 3)")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected stat error: %v", statErr)
	}
}

// ============================================================================
// P1 (CLI/lock-order audit) — torn-shim invariant. The per-OS autostart
// backends are NOT all-or-nothing: each mutates the shim BEFORE the OS step
// can error (Windows deletes the prior task before Create; Linux overwrites
// the unit before `systemctl enable --now`; macOS boots out + overwrites the
// plist before `launchctl bootstrap`). So a failed Enable does NOT prove the
// shim is untouched. When the intent revert succeeds but the shim cannot be
// PROVEN unchanged, strict-mode must KEEP a torn breadcrumb (never delete it)
// + exit 10 so `--recover` can drive both resources back in sync.
//
// These tests model the torn shim through the fake backend's statusFn seam:
// the snapshot taken before step 2 and the re-probe taken after the failed
// Enable disagree (or the probe errors), which is the exact torn signal.
// ============================================================================

// tornShimStatusFn returns a statusFn whose Nth call yields states[N] (clamped
// to the last element). It lets a test drive the pre-step2 snapshot and the
// post-failure re-probe to different States.
func tornShimStatusFn(states ...autostart.State) func(autostart.Options) (autostart.State, error) {
	call := 0
	return func(autostart.Options) (autostart.State, error) {
		idx := call
		if idx >= len(states) {
			idx = len(states) - 1
		}
		call++
		return states[idx], nil
	}
}

// tornShimSpecFn returns a liveness-free synthetic shim spec for each Status
// probe. Tests use it to distinguish drifted-A from drifted-B while keeping
// both states at StateDrifted.
func tornShimSpecFn(specs ...string) func(autostart.Options) string {
	call := 0
	return func(autostart.Options) string {
		idx := call
		if idx >= len(specs) {
			idx = len(specs) - 1
		}
		call++
		return specs[idx]
	}
}

// TestStrictModeEnable_TornShimKeepsBreadcrumb is the P1 falsifying regression.
// Pre-fix: Enable fails, intent revert succeeds, and the code BLINDLY deletes
// the breadcrumb and reports clean — even though the shim is torn. Post-fix the
// re-probe disagrees with the snapshot, so the breadcrumb is KEPT + exit 10.
func TestStrictModeEnable_TornShimKeepsBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.MakeShimWriteFail()
	// Snapshot (call 1) = EnabledRunning; re-probe after failed Enable (call 2)
	// = Absent. This is the Windows torn signature: the prior task was deleted
	// before Create failed.
	tmp.backend.statusFn = tornShimStatusFn(autostart.StateEnabledRunning, autostart.StateAbsent)

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeRevertFailed {
		t.Fatalf("expected exit %d (STRICT_MODE_REVERT_FAILED) on torn shim, got %d (err=%v)", ExitStrictModeRevertFailed, exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
		t.Fatalf("breadcrumb MUST survive a torn shim (the invariant); missing: %v", statErr)
	}
	bc := tmp.ReadBreadcrumb()
	if bc.Phase != strictModeBreadcrumbPhaseTorn {
		t.Errorf("breadcrumb.phase = %q on torn shim, want %q", bc.Phase, strictModeBreadcrumbPhaseTorn)
	}
	if bc.Intended != true {
		t.Errorf("breadcrumb.intended = %v, want true (enable)", bc.Intended)
	}
	// Intent revert succeeded → intent is back at original false.
	if bc.ActualIntentState != false {
		t.Errorf("breadcrumb.actual_intent_state = %v, want false (intent reverted)", bc.ActualIntentState)
	}
	if bc.Step2Error == "" {
		t.Error("breadcrumb missing step2_error")
	}
	// Intent really was reverted (the revert write itself succeeded here).
	intent, readErr := api.ReadSupervisorIntent(tmp.IntentPath())
	if readErr != nil {
		t.Fatalf("read intent: %v", readErr)
	}
	if intent.StrictMode {
		t.Error("intent.strict_mode not reverted to false after torn-shim failure")
	}
}

func TestStrictModeEnable_DriftedSpecChangeKeepsBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.SeedInitialStrict(false)
	tmp.MakeShimWriteFail()
	tmp.backend.statusFn = tornShimStatusFn(autostart.StateDrifted, autostart.StateDrifted)
	tmp.backend.statusSpecFn = tornShimSpecFn(
		`installed=true enabled=true command=C:\mcp\mcphub.exe args=gui`,
		`installed=true enabled=true command=C:\mcp\mcphub.exe args=gui --strict-mode`,
	)

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeRevertFailed {
		t.Fatalf("drifted-A to drifted-B must force exit %d, got %d (err=%v)", ExitStrictModeRevertFailed, exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
		t.Fatalf("breadcrumb must survive drifted-A to drifted-B; missing: %v", statErr)
	}
}

func TestStrictModeEnable_IdenticalDriftedSpecDeletesBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.SeedInitialStrict(false)
	tmp.MakeShimWriteFail()
	tmp.backend.statusFn = tornShimStatusFn(autostart.StateDrifted, autostart.StateDrifted)
	tmp.backend.statusSpecFn = tornShimSpecFn(
		`installed=true enabled=true command=C:\mcp\mcphub.exe args=legacy-supervise`,
		`installed=true enabled=true command=C:\mcp\mcphub.exe args=legacy-supervise`,
	)

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if err == nil {
		t.Fatal("expected non-nil error surfacing the shim Enable failure")
	}
	if exitCode := exitCodeFromError(err); exitCode == ExitStrictModeRevertFailed {
		t.Fatalf("identical drifted spec must not force recovery: exit %d (err=%v)", exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("breadcrumb must be deleted when the drifted shim spec is unchanged")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected breadcrumb stat error: %v", statErr)
	}
}

func TestStrictModeEnable_DriftedLivenessOnlyChangeDeletesBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.SeedInitialStrict(false)
	tmp.MakeShimWriteFail()
	tmp.backend.statusFn = tornShimStatusFn(autostart.StateDrifted, autostart.StateDrifted)
	tmp.backend.statusSpecFn = tornShimSpecFn(
		`installed=true enabled=true command=C:\mcp\mcphub.exe args=legacy-supervise`,
		`installed=true enabled=true command=C:\mcp\mcphub.exe args=legacy-supervise`,
	)

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if err == nil {
		t.Fatal("expected non-nil error surfacing the shim Enable failure")
	}
	if exitCode := exitCodeFromError(err); exitCode == ExitStrictModeRevertFailed {
		t.Fatalf("liveness-only drifted reprobe change must not force recovery: exit %d (err=%v)", exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("breadcrumb must be deleted when only drifted-shim liveness changed")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected breadcrumb stat error: %v", statErr)
	}
}

func TestStrictModeShimDriftFingerprintEnabledMatchingComparesSpec(t *testing.T) {
	matchingA := strictModeShimDriftFingerprint(autostart.StatusSnapshot{
		State:           autostart.StateEnabledRunning,
		SpecFingerprint: "installed=true enabled=true args=gui",
	})
	matchingB := strictModeShimDriftFingerprint(autostart.StatusSnapshot{
		State:           autostart.StateEnabledStopped,
		SpecFingerprint: "installed=true enabled=true args=gui",
	})
	if matchingA != matchingB {
		t.Fatalf("identical enabled-matching specs compared different: running=%v stopped=%v", matchingA, matchingB)
	}
	if matchingA.spec == "" {
		t.Fatal("enabled-matching fingerprint dropped the populated shim spec")
	}

	changed := strictModeShimDriftFingerprint(autostart.StatusSnapshot{
		State:           autostart.StateEnabledRunning,
		SpecFingerprint: "installed=true enabled=true args=gui --strict-mode",
	})
	if changed == matchingA {
		t.Fatalf("different enabled-matching specs collapsed to unchanged: base=%v changed=%v", matchingA, changed)
	}
}

// TestStrictModeEnable_LivenessOnlyReprobeChangeDeletesBreadcrumb pins the
// drift fingerprint distinction: enabled-running and enabled-stopped differ
// only by supervisor liveness, not by the installed shim shape. A liveness tick
// between the snapshot and re-probe must not force recovery.
func TestStrictModeEnable_LivenessOnlyReprobeChangeDeletesBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.SeedInitialStrict(false)
	tmp.MakeShimWriteFail()
	tmp.backend.statusFn = tornShimStatusFn(autostart.StateEnabledRunning, autostart.StateEnabledStopped)

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if err == nil {
		t.Fatal("expected non-nil error surfacing the shim Enable failure")
	}
	if exitCode := exitCodeFromError(err); exitCode == ExitStrictModeRevertFailed {
		t.Fatalf("liveness-only status change forced recovery: exit %d (err=%v)", exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("breadcrumb must be deleted when only shim liveness changed")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected breadcrumb stat error: %v", statErr)
	}
}

// TestStrictModeEnable_RealShimDriftKeepsBreadcrumb is the negative control for
// the liveness-only case: a re-probe that reports drifted means the shim args or
// binary no longer match the original strict-mode fingerprint, so recovery must
// stay forced.
func TestStrictModeEnable_RealShimDriftKeepsBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.SeedInitialStrict(false)
	tmp.MakeShimWriteFail()
	tmp.backend.statusFn = tornShimStatusFn(autostart.StateEnabledRunning, autostart.StateDrifted)

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeRevertFailed {
		t.Fatalf("real shim drift must force exit %d, got %d (err=%v)", ExitStrictModeRevertFailed, exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
		t.Fatalf("breadcrumb must survive real shim drift; missing: %v", statErr)
	}
}

// TestStrictModeEnable_ReprobeErrorKeepsBreadcrumb proves an UNPROVABLE shim
// (the re-probe itself errors) is treated as torn — breadcrumb KEPT, exit 10.
// "Cannot prove unchanged" is fail-closed, not fail-open.
func TestStrictModeEnable_ReprobeErrorKeepsBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.MakeShimWriteFail()
	call := 0
	tmp.backend.statusFn = func(autostart.Options) (autostart.State, error) {
		call++
		if call == 1 {
			return autostart.StateEnabledRunning, nil // snapshot OK
		}
		return autostart.StateAbsent, errors.New("simulated re-probe Status failure")
	}

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeRevertFailed {
		t.Fatalf("expected exit %d when re-probe errors, got %d (err=%v)", ExitStrictModeRevertFailed, exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
		t.Fatalf("breadcrumb MUST survive an unprovable shim (re-probe error); missing: %v", statErr)
	}
}

// TestStrictModeEnable_SnapshotErrorKeepsBreadcrumb proves that when the
// PRE-step2 snapshot itself could not be taken, a subsequent Enable failure is
// also treated as unprovable → breadcrumb KEPT, exit 10. Without a baseline
// snapshot there is no evidence the shim is unchanged.
func TestStrictModeEnable_SnapshotErrorKeepsBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.MakeShimWriteFail()
	tmp.backend.statusFn = func(autostart.Options) (autostart.State, error) {
		return autostart.StateAbsent, errors.New("simulated snapshot Status failure")
	}

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeRevertFailed {
		t.Fatalf("expected exit %d when snapshot errors, got %d (err=%v)", ExitStrictModeRevertFailed, exitCode, err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
		t.Fatalf("breadcrumb MUST survive an unprovable shim (snapshot error); missing: %v", statErr)
	}
}

// TestStrictModeEnable_ProvenUnchangedShimDeletesBreadcrumb pins the positive
// half of the invariant: when the failed Enable genuinely left the shim
// untouched (snapshot == re-probe, both clean), the breadcrumb IS deleted and
// the command does NOT exit 10 — a torn-shim guard must not over-fire and force
// a spurious --recover on every shim failure. The shim starts INSTALLED so the
// proven-unchanged value is EnabledRunning (not the trivial Absent case the
// fresh-fixture revert tests already cover).
func TestStrictModeEnable_ProvenUnchangedShimDeletesBreadcrumb(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.SeedInitialStrict(false) // installed, strict=false
	tmp.MakeShimWriteFail()
	// Both snapshot and re-probe return the same clean state → provably
	// unchanged.
	tmp.backend.statusFn = tornShimStatusFn(autostart.StateEnabledRunning, autostart.StateEnabledRunning)

	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if err == nil {
		t.Fatal("expected non-nil error surfacing the shim Enable failure")
	}
	if exitCode := exitCodeFromError(err); exitCode == ExitStrictModeRevertFailed {
		t.Fatalf("torn-shim guard over-fired: exit 10 on a provably-unchanged shim (err=%v)", err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("breadcrumb must be deleted when the shim is provably unchanged (no torn state)")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected stat error: %v", statErr)
	}
}

func TestStrictModeEnable_SnapshotTakenBeforeIntentMutation(t *testing.T) {
	tmp := setupSupervisorFixture(t)

	statusCalls := 0
	var strictModeSeenBySnapshot bool
	tmp.backend.statusFn = func(autostart.Options) (autostart.State, error) {
		statusCalls++
		if statusCalls == 1 {
			intent, err := api.ReadSupervisorIntent(tmp.IntentPath())
			if err != nil {
				t.Fatalf("read intent from Status snapshot: %v", err)
			}
			strictModeSeenBySnapshot = intent.StrictMode
		}
		return autostart.StateEnabledStopped, nil
	}

	if err := RunStrictMode([]string{"enable"}, tmp.Deps()); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if statusCalls == 0 {
		t.Fatal("Status snapshot was not taken")
	}
	if strictModeSeenBySnapshot {
		t.Fatal("Status snapshot observed strict_mode=true; snapshot must be taken before the intent mutation")
	}
}

// TestStrictModeEnable_TornShimPerPlatformSignatures models each per-OS
// backend's torn signature through the snapshot/re-probe pair and asserts the
// breadcrumb is kept + exit 10 for every one. This is the cross-platform
// coverage the audit asked for (the real backends are platform-split behind
// build tags; the fake reproduces their observable torn state transition).
func TestStrictModeEnable_TornShimPerPlatformSignatures(t *testing.T) {
	cases := []struct {
		name              string
		snapshot, reprobe autostart.State
	}{
		// Windows: Delete succeeded, Create failed → task now Absent.
		{"windows-delete-before-create", autostart.StateEnabledRunning, autostart.StateAbsent},
		// Linux: unit file overwritten with the NEW strict flag before
		// `systemctl enable --now` failed → probing with the ORIGINAL flag
		// now sees Drifted.
		{"linux-write-before-enable", autostart.StateEnabledRunning, autostart.StateDrifted},
		// macOS: prior agent booted out + plist overwritten with the NEW
		// flag before `launchctl bootstrap` failed → Drifted under the
		// original-flag probe.
		{"darwin-bootout-write-before-bootstrap", autostart.StateEnabledRunning, autostart.StateDrifted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := setupSupervisorFixture(t)
			tmp.SeedInitialStrict(false)
			tmp.MakeShimWriteFail()
			tmp.backend.statusFn = tornShimStatusFn(tc.snapshot, tc.reprobe)

			err := RunStrictMode([]string{"enable"}, tmp.Deps())
			if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeRevertFailed {
				t.Fatalf("%s: expected exit %d, got %d (err=%v)", tc.name, ExitStrictModeRevertFailed, exitCode, err)
			}
			if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
				t.Fatalf("%s: breadcrumb must survive torn shim; missing: %v", tc.name, statErr)
			}
		})
	}
}

// TestStrictModeEnable_InProgressBreadcrumbDeletedAfterStep1Failure pins that
// a failure in the initial intent write does not leave an in-progress
// breadcrumb behind. Since step 1 failed, neither resource changed; leaving the
// marker would force a spurious --recover before future strict-mode attempts.
func TestStrictModeEnable_InProgressBreadcrumbDeletedAfterStep1Failure(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	deps := tmp.Deps()
	deps.MutateIntentFn = func(_ string, _ func(*api.SupervisorIntentFile) (bool, error)) error {
		return errors.New("simulated initial intent write failure")
	}

	err := RunStrictMode([]string{"enable"}, deps)
	if err == nil {
		t.Fatal("expected initial intent write failure")
	}
	if !strings.Contains(err.Error(), "simulated initial intent write failure") {
		t.Fatalf("expected step 1 write error to surface, got %v", err)
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("in-progress breadcrumb survived a failed initial intent write")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected stat error: %v", statErr)
	}
	intent, readErr := api.ReadSupervisorIntent(tmp.IntentPath())
	if readErr != nil {
		t.Fatalf("read intent: %v", readErr)
	}
	if intent.StrictMode {
		t.Fatal("intent.strict_mode changed even though the initial write failed")
	}
	if len(tmp.backend.enableCalls) != 0 {
		t.Fatalf("shim enable called %d times even though step 1 failed", len(tmp.backend.enableCalls))
	}

	// A subsequent invocation should reach the normal mutation path instead of
	// refusing on a stale breadcrumb.
	if retryErr := RunStrictMode([]string{"enable"}, tmp.Deps()); retryErr != nil {
		t.Fatalf("retry after failed step 1 should not be blocked by stale breadcrumb: %v", retryErr)
	}
}

// TestStrictModeEnable_InProgressBreadcrumbKeptAfterLateStep1Error pins that
// a write error returned after the intent file was published must not delete
// the only recovery marker. The production atomic writer can fail during
// post-rename close/re-open/verification, after strict_mode already changed.
func TestStrictModeEnable_InProgressBreadcrumbKeptAfterLateStep1Error(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	deps := tmp.Deps()
	deps.MutateIntentFn = func(path string, mutate func(*api.SupervisorIntentFile) (bool, error)) error {
		if err := api.MutateSupervisorIntentIfChanged(path, mutate); err != nil {
			return err
		}
		return errors.New("simulated late post-rename verification failure")
	}

	err := RunStrictMode([]string{"enable"}, deps)
	if err == nil {
		t.Fatal("expected late step 1 write failure")
	}
	if !strings.Contains(err.Error(), "simulated late post-rename verification failure") {
		t.Fatalf("expected late write error to surface, got %v", err)
	}
	intent, readErr := api.ReadSupervisorIntent(tmp.IntentPath())
	if readErr != nil {
		t.Fatalf("read intent: %v", readErr)
	}
	if !intent.StrictMode {
		t.Fatal("test setup did not publish strict_mode=true before returning late error")
	}
	if len(tmp.backend.enableCalls) != 0 {
		t.Fatalf("shim enable called %d times even though step 1 returned an error", len(tmp.backend.enableCalls))
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr != nil {
		t.Fatalf("in-progress breadcrumb missing after ambiguous late step 1 error: %v", statErr)
	}

	deps.MutateIntentFn = nil
	deps.PromptOperator = func() (string, error) { return "B", nil }
	if recoverErr := RunStrictModeRecover(deps); recoverErr != nil {
		t.Fatalf("recover after late step 1 error: %v", recoverErr)
	}
	intent, readErr = api.ReadSupervisorIntent(tmp.IntentPath())
	if readErr != nil {
		t.Fatalf("read recovered intent: %v", readErr)
	}
	if intent.StrictMode {
		t.Fatal("recover branch B did not roll intent back to original strict_mode=false")
	}
	if tmp.backend.currentStrict {
		t.Fatal("recover branch B unexpectedly left shim strict_mode=true")
	}
	if _, statErr := os.Stat(tmp.BreadcrumbPath()); statErr == nil {
		t.Fatal("breadcrumb not deleted after successful recover")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected breadcrumb stat error after recover: %v", statErr)
	}
}

// TestStrictModeRecover_InProgressMarkerReconciles proves the --recover surface
// consumes the in-progress marker left by a simulated SIGKILL in the step1→step2
// window. The crash state: intent flipped to desired (true), shim still at
// original (false). Branch B drives both back to the recorded pre-mutation
// ActualIntentState (false), reconciling the drift; the breadcrumb is deleted.
func TestStrictModeRecover_InProgressMarkerReconciles(t *testing.T) {
	tmp := setupSupervisorFixture(t)

	// Simulate the crash residue directly: an in-progress breadcrumb plus the
	// post-step1 / pre-step2 on-disk state (intent=true, shim=false).
	inProgress := strictModeBreadcrumb{
		Intended:          true,  // operator ran `enable`
		ActualIntentState: false, // pre-mutation value (rollback target)
		ActualShimState:   false, // shim never flipped (step 2 never ran)
		TS:                "2026-06-13T12:00:00Z",
		Phase:             strictModeBreadcrumbPhaseInProgress,
	}
	raw, _ := json.MarshalIndent(inProgress, "", "  ")
	if err := os.WriteFile(tmp.BreadcrumbPath(), raw, 0o600); err != nil {
		t.Fatalf("seed in-progress breadcrumb: %v", err)
	}
	// Live state mirrors the crash: intent flipped to true, shim still false.
	tmp.SeedInitialStrict(true)
	tmp.backend.installed = true
	tmp.backend.currentStrict = false

	// Sanity: a normal enable/disable invocation must REFUSE while the
	// in-progress marker is present (same refuse-if-held surface).
	if refuseErr := RunStrictMode([]string{"disable"}, tmp.Deps()); refuseErr == nil {
		t.Fatal("strict-mode must refuse while an in-progress breadcrumb is present; got nil")
	} else if !strings.Contains(refuseErr.Error(), "--recover") {
		t.Errorf("refusal should point at --recover; got %v", refuseErr)
	}

	deps := tmp.Deps()
	deps.PromptOperator = func() (string, error) { return "B", nil }
	if err := RunStrictModeRecover(deps); err != nil {
		t.Fatalf("recover from in-progress marker: %v", err)
	}
	// Branch B reconciled both resources to actual_intent_state=false.
	intent, _ := api.ReadSupervisorIntent(tmp.IntentPath())
	if intent.StrictMode {
		t.Errorf("intent.strict_mode = true after branch B recover, want false")
	}
	if tmp.backend.currentStrict {
		t.Errorf("shim.strict_mode = true after branch B recover, want false")
	}
	if _, err := os.Stat(tmp.BreadcrumbPath()); err == nil {
		t.Error("in-progress breadcrumb not deleted after successful recover")
	}
}

func TestStrictModeRecover_BreadcrumbReadBypassesPersistedStrictMode(t *testing.T) {
	t.Setenv(api.RequireSingleUserHomeEnv, "")
	stateDir := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(stateDir, 0o755); err != nil {
			t.Fatalf("chmod state dir read-broadened: %v", err)
		}
	}
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	api.ResetStrictModeIntentCacheForTest()
	t.Cleanup(api.ResetStrictModeIntentCacheForTest)

	tmp := &strictModeFixture{
		t:              t,
		stateDir:       stateDir,
		intentPath:     filepath.Join(stateDir, "supervisor-intent.json"),
		breadcrumbPath: filepath.Join(stateDir, "strict-mode-mutation-incomplete.json"),
		backend:        &fakeAutostartBackend{},
	}
	timerEnabled := true
	initial := &api.SupervisorIntentFile{
		Version:   11,
		UpdatedAt: "1970-01-01T00:00:00Z",
		Daemons: []api.SupervisorDaemon{{
			TaskName:     `\mcp-local-hub-memory-default`,
			Server:       "memory",
			Daemon:       "default",
			Command:      "node",
			Args:         []string{"memory-server.js"},
			Port:         9128,
			ManifestHash: "sha256:memory",
		}},
		MaintenanceTimers: []api.MaintenanceTimer{{
			Name:    `\mcp-local-hub-maintenance-memory`,
			Kind:    "server-weekly-refresh",
			Server:  "memory",
			Command: "mcphub",
			Args:    []string{"maintenance", "refresh", "memory"},
			Enabled: &timerEnabled,
		}},
		StrictMode: false,
		Stops: map[string]api.DaemonIntent{
			`\mcp-local-hub-memory-default`: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC),
			},
		},
	}
	if err := api.WriteSupervisorIntent(tmp.intentPath, initial); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	bc := strictModeBreadcrumb{
		Intended:          true,
		ActualIntentState: false,
		ActualShimState:   false,
		TS:                "2026-06-20T10:00:00Z",
		Phase:             strictModeBreadcrumbPhaseInProgress,
	}
	raw, _ := json.MarshalIndent(bc, "", "  ")
	if err := api.WriteStateFileBytesAtomic(tmp.BreadcrumbPath(), raw); err != nil {
		t.Fatalf("seed breadcrumb: %v", err)
	}
	liveIntent := *initial
	liveIntent.StrictMode = true
	if err := api.WriteSupervisorIntent(tmp.IntentPath(), &liveIntent); err != nil {
		t.Fatalf("seed live strict intent: %v", err)
	}
	tmp.backend.installed = true
	tmp.backend.currentStrict = false
	api.ResetStrictModeIntentCacheForTest()

	deps := tmp.Deps()
	deps.PromptOperator = func() (string, error) { return "B", nil }
	if err := RunStrictModeRecover(deps); err != nil {
		t.Fatalf("recover must read breadcrumb even when persisted strict_mode=true and parent is read-broadened: %v", err)
	}
	api.ResetStrictModeIntentCacheForTest()
	intent, err := api.ReadSupervisorIntent(tmp.IntentPath())
	if err != nil {
		t.Fatalf("read recovered intent: %v", err)
	}
	if intent.StrictMode {
		t.Fatal("recover branch B did not roll intent back to strict_mode=false")
	}
	if tmp.backend.currentStrict {
		t.Fatal("recover branch B unexpectedly left shim strict_mode=true")
	}
	if intent.Version != initial.Version {
		t.Fatalf("version = %d, want %d", intent.Version, initial.Version)
	}
	if intent.UpdatedAt != initial.UpdatedAt {
		t.Fatalf("updated_at = %q, want %q", intent.UpdatedAt, initial.UpdatedAt)
	}
	if !reflect.DeepEqual(intent.Daemons, initial.Daemons) {
		t.Fatalf("daemons changed: got %+v want %+v", intent.Daemons, initial.Daemons)
	}
	if !reflect.DeepEqual(intent.MaintenanceTimers, initial.MaintenanceTimers) {
		t.Fatalf("maintenance_timers changed: got %+v want %+v", intent.MaintenanceTimers, initial.MaintenanceTimers)
	}
	if !reflect.DeepEqual(intent.Stops, initial.Stops) {
		t.Fatalf("stops changed: got %+v want %+v", intent.Stops, initial.Stops)
	}
}

// ============================================================================
// Disable path (parallel to enable): true → false atomic mutation.
// ============================================================================

func TestStrictModeDisable_AtomicTwoResource(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	tmp.SeedInitialStrict(true)

	if err := RunStrictMode([]string{"disable"}, tmp.Deps()); err != nil {
		t.Fatalf("disable: %v", err)
	}
	intent, err := api.ReadSupervisorIntent(tmp.IntentPath())
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if intent.StrictMode {
		t.Fatal("intent.strict_mode should be false after disable")
	}
	shimArgs := tmp.ReadShimArgs()
	if strings.Contains(shimArgs, "--strict-mode") {
		t.Fatalf("shim args should not contain --strict-mode after disable: got %q", shimArgs)
	}
}

func TestStrictModeDisable_PreservesNonStrictIntentFields(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	enabled := false
	original := &api.SupervisorIntentFile{
		Version:   7,
		UpdatedAt: "2026-06-20T12:34:56Z",
		Daemons: []api.SupervisorDaemon{{
			TaskName:     `\mcp-local-hub-time-default`,
			Server:       "time",
			Daemon:       "default",
			Command:      "mcphub",
			Args:         []string{"daemon", "--server", "time"},
			Port:         9129,
			ManifestHash: "sha256:time",
		}},
		MaintenanceTimers: []api.MaintenanceTimer{{
			Name:    `\mcp-local-hub-maintenance-time`,
			Kind:    "server-weekly-refresh",
			Server:  "time",
			Command: "mcphub",
			Args:    []string{"maintenance", "refresh", "time"},
			Enabled: &enabled,
		}},
		StrictMode: true,
		Stops: map[string]api.DaemonIntent{
			`\mcp-local-hub-time-default`: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	if err := api.WriteSupervisorIntent(tmp.IntentPath(), original); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	tmp.backend.installed = true
	tmp.backend.currentStrict = true

	if err := RunStrictMode([]string{"disable"}, tmp.Deps()); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, err := api.ReadSupervisorIntent(tmp.IntentPath())
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if got.StrictMode {
		t.Fatal("strict_mode should be false after disable")
	}
	if got.Version != original.Version {
		t.Fatalf("version = %d, want %d", got.Version, original.Version)
	}
	if got.UpdatedAt != original.UpdatedAt {
		t.Fatalf("updated_at = %q, want %q", got.UpdatedAt, original.UpdatedAt)
	}
	if !reflect.DeepEqual(got.Daemons, original.Daemons) {
		t.Fatalf("daemons changed: got %+v want %+v", got.Daemons, original.Daemons)
	}
	if !reflect.DeepEqual(got.MaintenanceTimers, original.MaintenanceTimers) {
		t.Fatalf("maintenance_timers changed: got %+v want %+v", got.MaintenanceTimers, original.MaintenanceTimers)
	}
	if !reflect.DeepEqual(got.Stops, original.Stops) {
		t.Fatalf("stops changed: got %+v want %+v", got.Stops, original.Stops)
	}
}

func TestStrictModeEnablePreservesConcurrentIntentWriter(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	path := tmp.IntentPath()

	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock supervisor intent: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- RunStrictMode([]string{"enable"}, tmp.Deps())
	}()

	select {
	case err := <-done:
		_ = lock.Unlock()
		t.Fatalf("RunStrictMode returned before the flock holder published the concurrent descriptor; err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	concurrent := &api.SupervisorIntentFile{
		Version:    1,
		UpdatedAt:  "2026-06-30T00:00:00Z",
		StrictMode: false,
		Daemons: []api.SupervisorDaemon{{
			TaskName: `\mcp-local-hub-serena-concurrent`,
			Server:   "serena",
			Daemon:   "default",
			Command:  "mcphub",
			Args:     []string{"daemon", "serena-proxy"},
			Port:     9123,
		}},
	}
	raw, err := json.MarshalIndent(concurrent, "", "  ")
	if err != nil {
		_ = lock.Unlock()
		t.Fatalf("marshal concurrent intent: %v", err)
	}
	if err := api.WriteStateFileBytesLockHeld(path, raw); err != nil {
		_ = lock.Unlock()
		t.Fatalf("publish concurrent intent under held flock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock supervisor intent: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("RunStrictMode after flock release: %v", err)
	}
	got, err := api.ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read supervisor intent: %v", err)
	}
	if !got.StrictMode {
		t.Fatal("strict_mode = false, want true from strict-mode enable")
	}
	if len(got.Daemons) != 1 || got.Daemons[0].TaskName != `\mcp-local-hub-serena-concurrent` {
		t.Fatalf("concurrent daemon row was not preserved: %+v", got.Daemons)
	}
}

// ============================================================================
// Recover — branch A drives both to original intended.
// ============================================================================

func TestStrictModeRecover_BranchA(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	// Pre-stage breadcrumb representing a failed enable: operator
	// intended true, intent flipped to true (step 1 succeeded), but
	// shim never got flipped (step 2 failed AND revert failed).
	bc := strictModeBreadcrumb{
		Intended:          true,
		ActualIntentState: true,
		ActualShimState:   false,
		Step1Error:        "",
		Step2Error:        "shim Enable failed",
		RevertError:       "WriteSupervisorIntent rename failed",
		TS:                "2026-05-17T12:00:00Z",
	}
	raw, _ := json.MarshalIndent(bc, "", "  ")
	if err := os.WriteFile(tmp.BreadcrumbPath(), raw, 0o600); err != nil {
		t.Fatalf("seed breadcrumb: %v", err)
	}
	// Set live state to match breadcrumb's recorded actual: intent=true,
	// shim=false.
	tmp.SeedInitialStrict(true)
	tmp.backend.installed = true
	tmp.backend.currentStrict = false

	deps := tmp.Deps()
	deps.PromptOperator = func() (string, error) { return "A", nil }

	if err := RunStrictModeRecover(deps); err != nil {
		t.Fatalf("recover: %v", err)
	}
	// Branch A drives BOTH to intended=true.
	intent, _ := api.ReadSupervisorIntent(tmp.IntentPath())
	if !intent.StrictMode {
		t.Errorf("intent.strict_mode = false after branch A (want true)")
	}
	if !tmp.backend.currentStrict {
		t.Errorf("shim.strict_mode = false after branch A (want true)")
	}
	// Breadcrumb must be deleted on success.
	if _, err := os.Stat(tmp.BreadcrumbPath()); err == nil {
		t.Error("breadcrumb not deleted after successful recover")
	}
}

func TestStrictModeRecover_PreservesNonStrictIntentFields(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	enabled := true
	original := &api.SupervisorIntentFile{
		Version:   9,
		UpdatedAt: "2026-06-20T13:00:00Z",
		Daemons: []api.SupervisorDaemon{{
			TaskName:     `\mcp-local-hub-memory-default`,
			Server:       "memory",
			Daemon:       "default",
			Command:      "node",
			Args:         []string{"./mcp-memory-server.js"},
			Port:         9128,
			ManifestHash: "sha256:memory",
		}},
		MaintenanceTimers: []api.MaintenanceTimer{{
			Name:    `\mcp-local-hub-maintenance-memory`,
			Kind:    "server-weekly-refresh",
			Server:  "memory",
			Command: "mcphub",
			Args:    []string{"maintenance", "refresh", "memory"},
			Enabled: &enabled,
		}},
		StrictMode: false,
		Stops: map[string]api.DaemonIntent{
			`\mcp-local-hub-memory-default`: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC),
			},
		},
	}
	if err := api.WriteSupervisorIntent(tmp.IntentPath(), original); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	tmp.backend.installed = true
	tmp.backend.currentStrict = false
	bc := strictModeBreadcrumb{
		Intended:          true,
		ActualIntentState: false,
		ActualShimState:   false,
		Step2Error:        "post-step1 shim install error",
		RevertError:       "revert write hit ENOSPC",
		TS:                "2026-06-20T13:01:00Z",
	}
	raw, _ := json.MarshalIndent(bc, "", "  ")
	if err := os.WriteFile(tmp.BreadcrumbPath(), raw, 0o600); err != nil {
		t.Fatalf("seed breadcrumb: %v", err)
	}

	deps := tmp.Deps()
	deps.PromptOperator = func() (string, error) { return "A", nil }
	if err := RunStrictModeRecover(deps); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, err := api.ReadSupervisorIntent(tmp.IntentPath())
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if !got.StrictMode {
		t.Fatal("strict_mode should be true after branch A recover")
	}
	if got.Version != original.Version {
		t.Fatalf("version = %d, want %d", got.Version, original.Version)
	}
	if got.UpdatedAt != original.UpdatedAt {
		t.Fatalf("updated_at = %q, want %q", got.UpdatedAt, original.UpdatedAt)
	}
	if !reflect.DeepEqual(got.Daemons, original.Daemons) {
		t.Fatalf("daemons changed: got %+v want %+v", got.Daemons, original.Daemons)
	}
	if !reflect.DeepEqual(got.MaintenanceTimers, original.MaintenanceTimers) {
		t.Fatalf("maintenance_timers changed: got %+v want %+v", got.MaintenanceTimers, original.MaintenanceTimers)
	}
	if !reflect.DeepEqual(got.Stops, original.Stops) {
		t.Fatalf("stops changed: got %+v want %+v", got.Stops, original.Stops)
	}
}

// ============================================================================
// Recover — branch B drives both to actual_intent_state.
// ============================================================================

func TestStrictModeRecover_BranchB(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	bc := strictModeBreadcrumb{
		Intended:          true,
		ActualIntentState: false,
		ActualShimState:   true,
		Step1Error:        "",
		Step2Error:        "post-step1 shim install error",
		RevertError:       "revert write hit ENOSPC",
		TS:                "2026-05-17T12:00:00Z",
	}
	raw, _ := json.MarshalIndent(bc, "", "  ")
	if err := os.WriteFile(tmp.BreadcrumbPath(), raw, 0o600); err != nil {
		t.Fatalf("seed breadcrumb: %v", err)
	}
	// Live state mirrors breadcrumb's record.
	tmp.SeedInitialStrict(false)
	tmp.backend.installed = true
	tmp.backend.currentStrict = true

	deps := tmp.Deps()
	deps.PromptOperator = func() (string, error) { return "B", nil }

	if err := RunStrictModeRecover(deps); err != nil {
		t.Fatalf("recover: %v", err)
	}
	// Branch B drives BOTH to actual_intent_state=false.
	intent, _ := api.ReadSupervisorIntent(tmp.IntentPath())
	if intent.StrictMode {
		t.Errorf("intent.strict_mode = true after branch B (want false)")
	}
	if tmp.backend.currentStrict {
		t.Errorf("shim.strict_mode = true after branch B (want false)")
	}
	if _, err := os.Stat(tmp.BreadcrumbPath()); err == nil {
		t.Error("breadcrumb not deleted after successful recover")
	}
}

// ============================================================================
// Refuse-if-breadcrumb-exists on enable/disable (operator must --recover).
// ============================================================================

func TestStrictMode_RefusesIfBreadcrumbExists(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	// Seed a breadcrumb to simulate a prior failed mutation.
	bc := strictModeBreadcrumb{
		Intended:          true,
		ActualIntentState: true,
		ActualShimState:   false,
		RevertError:       "stuck",
		TS:                "2026-05-17T12:00:00Z",
	}
	raw, _ := json.MarshalIndent(bc, "", "  ")
	if err := os.WriteFile(tmp.BreadcrumbPath(), raw, 0o600); err != nil {
		t.Fatalf("seed breadcrumb: %v", err)
	}
	err := RunStrictMode([]string{"enable"}, tmp.Deps())
	if err == nil {
		t.Fatal("expected refusal when breadcrumb exists, got nil")
	}
	if !strings.Contains(err.Error(), "--recover") {
		t.Errorf("refusal error should mention --recover; got %q", err.Error())
	}
	// Intent must be untouched.
	intent, _ := api.ReadSupervisorIntent(tmp.IntentPath())
	if intent.StrictMode {
		t.Error("intent.strict_mode was mutated despite refusal")
	}
}

// ============================================================================
// Lock-busy returns exit code 9.
// ============================================================================

func TestStrictMode_LockBusyReturns9(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	// Hold migration.lock externally to force the refuse-if-held path.
	migPath := filepath.Join(tmp.stateDir, "migration")
	holder, err := api.AcquireSupervisorLock(migPath)
	if err != nil {
		t.Fatalf("seed migration.lock holder: %v", err)
	}
	t.Cleanup(func() { holder.Release() })

	err = RunStrictMode([]string{"enable"}, tmp.Deps())
	if exitCode := exitCodeFromError(err); exitCode != ExitStrictModeBusy {
		t.Fatalf("expected exit %d (STRICT_MODE_BUSY), got %d (err=%v)", ExitStrictModeBusy, exitCode, err)
	}
}
