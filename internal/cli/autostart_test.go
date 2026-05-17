// CLI integration test for `mcphub autostart {enable|disable|status}`.
// Drives the cobra surface with a swapped-in fake autostart.Backend so
// the real Task Scheduler / systemctl / launchctl are never touched.
package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

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

// withFakeBackend swaps the autostart factory and returns the fake so
// the test can inspect it. Restores the prior factory on cleanup.
func withFakeBackend(t *testing.T, fb *fakeBackend) {
	t.Helper()
	prev := autostartBackendFactoryFn
	autostartBackendFactoryFn = func() (autostart.Backend, error) { return fb, nil }
	t.Cleanup(func() { autostartBackendFactoryFn = prev })
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
	fb := &fakeBackend{}
	withFakeBackend(t, fb)

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"enable"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fb.enableCalls) != 1 {
		t.Fatalf("Enable calls = %d, want 1: %+v", len(fb.enableCalls), fb.enableCalls)
	}
	if fb.enableCalls[0].StrictMode {
		t.Errorf("Enable opts.StrictMode = true; default should be false")
	}
}

func TestAutostartEnableStrictMode(t *testing.T) {
	fb := &fakeBackend{}
	withFakeBackend(t, fb)

	cmd := newAutostartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"enable", "--strict-mode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fb.enableCalls) != 1 || !fb.enableCalls[0].StrictMode {
		t.Errorf("Enable opts = %+v, want StrictMode=true", fb.enableCalls)
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
