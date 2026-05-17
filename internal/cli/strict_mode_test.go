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
	"strings"
	"testing"

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
	failEnable    bool   // when true, Enable returns shimErr
	shimErr       error  // err returned on shimFail path
	currentStrict bool   // last successful StrictMode the shim was set to
	installed     bool   // last successful Enable left the shim installed
}

func (f *fakeAutostartBackend) Enable(opts autostart.Options) error {
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

func (f *fakeAutostartBackend) Status(_ autostart.Options) (autostart.State, error) {
	if !f.installed {
		return autostart.StateAbsent, nil
	}
	return autostart.StateEnabledRunning, nil
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
// api.WriteSupervisorIntent through the deps.WriteIntentFn function
// pointer; revertFail flips that pointer to a stub that returns an
// error specifically on the revert call. We use a counter to
// distinguish step-1 write (succeeds) from revert write (fails).
func (f *strictModeFixture) MakeRevertWriteFail() {
	f.revertFail = true
}

// Deps assembles the StrictModeDeps the unit-under-test consumes. The
// WriteIntentFn override is the canonical seam — production deps leave
// it nil so RunStrictMode falls back to api.WriteSupervisorIntent.
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
		d.WriteIntentFn = func(path string, intent *api.SupervisorIntentFile) error {
			writes++
			if writes == 1 {
				// First write (step 1) succeeds.
				return api.WriteSupervisorIntent(path, intent)
			}
			// Subsequent writes (the revert) fail.
			return errors.New("simulated revert write failure")
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
