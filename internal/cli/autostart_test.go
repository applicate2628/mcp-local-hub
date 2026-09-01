//go:build windows

// CLI integration test for `mcphub autostart {enable|disable|status}`.
// Drives the cobra surface with a swapped-in fake autostart.Backend so
// the real Task Scheduler / systemctl / launchctl are never touched.
package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/autostart"
)

// fakeBackend records each Enable/Disable/Status call and returns the
// values the test pre-loaded. Satisfies autostart.Backend so the CLI
// can swap it in via the autostartBackendFactoryFn seam.
type fakeBackend struct {
	enableCalls  []autostart.Options
	disableCalls int
	statusCalls  []autostart.Options
	statusReturn autostart.State
	statusErr    error
	enableErr    error
	disableErr   error
}

func (f *fakeBackend) Enable(opts autostart.Options) error {
	f.enableCalls = append(f.enableCalls, opts)
	return f.enableErr
}
func (f *fakeBackend) Disable() error {
	f.disableCalls++
	return f.disableErr
}
func (f *fakeBackend) Status(opts autostart.Options) (autostart.State, error) {
	f.statusCalls = append(f.statusCalls, opts)
	return f.statusReturn, f.statusErr
}
func (f *fakeBackend) StatusSnapshot(opts autostart.Options) (autostart.StatusSnapshot, error) {
	state, err := f.Status(opts)
	return autostart.StatusSnapshot{State: state}, err
}

// withFakeBackend swaps the autostart factory and returns the fake so
// the test can inspect it. Restores the prior factory on cleanup.
func withFakeBackend(t *testing.T, fb *fakeBackend) {
	t.Helper()
	prev := autostartBackendFactoryFn
	autostartBackendFactoryFn = func() (autostart.Backend, error) { return fb, nil }
	prevPolicy := autostartStatusOptionsFn
	autostartStatusOptionsFn = func() (autostart.Options, error) {
		return autostart.Options{OwnerMode: autostart.OwnerModeGUI}, nil
	}
	t.Cleanup(func() {
		autostartBackendFactoryFn = prev
		autostartStatusOptionsFn = prevPolicy
	})
}

func TestAutostartStatusPrintsState(t *testing.T) {
	fb := &fakeBackend{statusReturn: autostart.StateEnabledRunning}
	withFakeBackend(t, fb)

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "enabled-running") {
		t.Errorf("status output = %q, want substring %q", got, "enabled-running")
	}
	if len(fb.statusCalls) != 1 {
		t.Errorf("Status calls = %d, want 1", len(fb.statusCalls))
	}
}

func TestAutostartStatusSchedulerUnavailableIsExplicitAndSuccessful(t *testing.T) {
	fb := &fakeBackend{statusErr: autostart.ErrStatusObservationUnavailable}
	withFakeBackend(t, fb)

	cmd := newAutostartCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := stdout.String(); got != "unavailable\n" {
		t.Fatalf("stdout = %q, want unavailable state", got)
	}
	if got := stderr.String(); got != "AUTOSTART_SCHEDULER_UNAVAILABLE\n" {
		t.Fatalf("stderr = %q, want stable scheduler diagnostic", got)
	}
}

func TestAutostartStatusStrictModeFlagThreaded(t *testing.T) {
	fb := &fakeBackend{statusReturn: autostart.StateDrifted}
	withFakeBackend(t, fb)

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"status", "--strict-mode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fb.statusCalls) != 1 || !fb.statusCalls[0].StrictMode {
		t.Errorf("Status opts = %+v, want StrictMode=true", fb.statusCalls)
	}
	if !strings.Contains(out.String(), "drifted") {
		t.Errorf("status output = %q, want substring %q", out.String(), "drifted")
	}
}

func TestAutostartEnableCallsBackend(t *testing.T) {
	prevRunner := autostartOwnerModeRunnerFn
	var gotOwner autostart.OwnerMode
	var gotStrict bool
	autostartOwnerModeRunnerFn = func(owner autostart.OwnerMode, strict bool) error {
		gotOwner, gotStrict = owner, strict
		return nil
	}
	t.Cleanup(func() { autostartOwnerModeRunnerFn = prevRunner })
	prevPolicy := autostartStatusOptionsFn
	autostartStatusOptionsFn = func() (autostart.Options, error) {
		return autostart.Options{OwnerMode: autostart.OwnerModeSupervise, StrictMode: true}, nil
	}
	t.Cleanup(func() { autostartStatusOptionsFn = prevPolicy })

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"enable"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotOwner != autostart.OwnerModeSupervise || !gotStrict {
		t.Fatalf("plain enable policy owner=%q strict=%v, want persisted supervise,true", gotOwner, gotStrict)
	}
}

func TestAutostartEnableStrictMode(t *testing.T) {
	prevRunner := autostartOwnerModeRunnerFn
	var gotOwner autostart.OwnerMode
	var gotStrict bool
	autostartOwnerModeRunnerFn = func(owner autostart.OwnerMode, strict bool) error {
		gotOwner, gotStrict = owner, strict
		return nil
	}
	t.Cleanup(func() { autostartOwnerModeRunnerFn = prevRunner })
	prevPolicy := autostartStatusOptionsFn
	autostartStatusOptionsFn = func() (autostart.Options, error) {
		return autostart.Options{OwnerMode: autostart.OwnerModeSupervise, StrictMode: false}, nil
	}
	t.Cleanup(func() { autostartStatusOptionsFn = prevPolicy })

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"enable", "--strict-mode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotOwner != autostart.OwnerModeSupervise || !gotStrict {
		t.Fatalf("plain enable --strict-mode policy owner=%q strict=%v, want supervise,true", gotOwner, gotStrict)
	}
}

func TestAutostartStatusPlainUsesPersistedOwnerPolicy(t *testing.T) {
	fb := &fakeBackend{statusReturn: autostart.StateEnabledRunning}
	withFakeBackend(t, fb)
	prevPolicy := autostartStatusOptionsFn
	autostartStatusOptionsFn = func() (autostart.Options, error) {
		return autostart.Options{OwnerMode: autostart.OwnerModeSupervise, StrictMode: true}, nil
	}
	t.Cleanup(func() { autostartStatusOptionsFn = prevPolicy })

	cmd := newAutostartCmd()
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fb.statusCalls) != 1 || fb.statusCalls[0].OwnerMode != autostart.OwnerModeSupervise || !fb.statusCalls[0].StrictMode {
		t.Fatalf("plain status options=%+v, want persisted supervise,true", fb.statusCalls)
	}
}

func TestAutostartOwnerModeSequence_PlainEnableAndStatusPreserveSupervise(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreStateRoot := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreStateRoot)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version:    1,
		StrictMode: true,
		OwnerMode:  api.OwnerModeSupervise,
	}); err != nil {
		t.Fatalf("seed supervise intent: %v", err)
	}
	fb := &fakeBackend{statusReturn: autostart.StateEnabledRunning}
	withFakeBackend(t, fb)
	prevPolicy := autostartStatusOptionsFn
	autostartStatusOptionsFn = persistedAutostartStatusOptions
	t.Cleanup(func() { autostartStatusOptionsFn = prevPolicy })

	enable := newAutostartCmd()
	enable.SetArgs([]string{"enable"})
	if err := enable.Execute(); err != nil {
		t.Fatalf("plain enable: %v", err)
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read post-enable intent: %v", err)
	}
	if got := intent.EffectiveOwnerMode(); got != api.OwnerModeSupervise {
		t.Fatalf("plain enable changed owner mode to %q, want supervise", got)
	}
	if len(fb.enableCalls) != 1 || fb.enableCalls[0].OwnerMode != autostart.OwnerModeSupervise || !fb.enableCalls[0].StrictMode {
		t.Fatalf("plain enable task options=%+v, want supervise,true", fb.enableCalls)
	}

	status := newAutostartCmd()
	var out bytes.Buffer
	status.SetOut(&out)
	status.SetArgs([]string{"status"})
	if err := status.Execute(); err != nil {
		t.Fatalf("plain status: %v", err)
	}
	if got := out.String(); got != "enabled-running\n" {
		t.Fatalf("plain status output=%q, want enabled-running token", got)
	}
	if last := fb.statusCalls[len(fb.statusCalls)-1]; last.OwnerMode != autostart.OwnerModeSupervise || !last.StrictMode {
		t.Fatalf("plain status task probe=%+v, want supervise,true", last)
	}

	prevTaskState := ensureAliveOwnerTaskStateFn
	ensureAliveOwnerTaskStateFn = func(opts autostart.Options) (autostart.State, error) {
		if opts.OwnerMode != autostart.OwnerModeSupervise || !opts.StrictMode {
			t.Fatalf("ensure-alive verification options=%+v, want supervise,true", opts)
		}
		return autostart.StateEnabledRunning, nil
	}
	t.Cleanup(func() { ensureAliveOwnerTaskStateFn = prevTaskState })
	if mode, err := resolveEnsureAliveOwnerMode(stateDir); err != nil || mode != api.OwnerModeSupervise {
		t.Fatalf("ensure-alive mode=%q err=%v, want verified supervise", mode, err)
	}
}

func TestAutostartEnableOwnerModeUsesTransactionalRunner(t *testing.T) {
	var gotOwner autostart.OwnerMode
	var gotStrict bool
	prev := autostartOwnerModeRunnerFn
	autostartOwnerModeRunnerFn = func(owner autostart.OwnerMode, strict bool) error {
		gotOwner, gotStrict = owner, strict
		return nil
	}
	t.Cleanup(func() { autostartOwnerModeRunnerFn = prev })

	cmd := newAutostartCmd()
	cmd.SetArgs([]string{"enable", "--owner-mode", "supervise", "--strict-mode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotOwner != autostart.OwnerModeSupervise || !gotStrict {
		t.Fatalf("transaction input owner=%q strict=%v, want supervise,true", gotOwner, gotStrict)
	}
}

func TestAutostartEnableOwnerModePreservesPersistedStrictModeWhenFlagIsAbsent(t *testing.T) {
	prevRunner := autostartOwnerModeRunnerFn
	t.Cleanup(func() { autostartOwnerModeRunnerFn = prevRunner })
	prevPolicy := autostartStatusOptionsFn
	autostartStatusOptionsFn = func() (autostart.Options, error) {
		return autostart.Options{StrictMode: true, OwnerMode: autostart.OwnerModeGUI}, nil
	}
	t.Cleanup(func() { autostartStatusOptionsFn = prevPolicy })

	var captured bool
	autostartOwnerModeRunnerFn = func(_ autostart.OwnerMode, strict bool) error {
		captured = strict
		return nil
	}
	cmd := newAutostartCmd()
	cmd.SetArgs([]string{"enable", "--owner-mode", "supervise"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !captured {
		t.Fatalf("owner-mode transition strict=%v, want preserved true", captured)
	}
}

func TestAutostartStatusDetailsUsesPersistedPolicyWithoutChangingStateToken(t *testing.T) {
	fb := &fakeBackend{statusReturn: autostart.StateEnabledRunning}
	withFakeBackend(t, fb)
	prev := autostartStatusOptionsFn
	autostartStatusOptionsFn = func() (autostart.Options, error) {
		return autostart.Options{OwnerMode: autostart.OwnerModeSupervise, StrictMode: true}, nil
	}
	t.Cleanup(func() { autostartStatusOptionsFn = prev })

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--details"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fb.statusCalls) != 1 || fb.statusCalls[0].OwnerMode != autostart.OwnerModeSupervise || !fb.statusCalls[0].StrictMode {
		t.Fatalf("status options=%+v, want persisted supervise strict policy", fb.statusCalls)
	}
	if got := out.String(); got != "enabled-running\nowner-mode=supervise\nstrict-mode=true\n" {
		t.Fatalf("details output=%q", got)
	}
}

func TestAutostartDisableCallsBackend(t *testing.T) {
	fb := &fakeBackend{}
	withFakeBackend(t, fb)

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"disable"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fb.disableCalls != 1 {
		t.Errorf("Disable calls = %d, want 1", fb.disableCalls)
	}
}

func TestAutostartBackendErrorSurfacesAsCmdError(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))

	fb := &fakeBackend{enableErr: errors.New("schtasks /Create: COM init failed")}
	withFakeBackend(t, fb)

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"enable"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute returned nil; want backend error propagated")
	}
	if !strings.Contains(err.Error(), "schtasks") {
		t.Errorf("err = %v, want backend message bubbled up", err)
	}
}
