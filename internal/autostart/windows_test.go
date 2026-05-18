//go:build windows

// Windows autostart backend tests. Every test injects a fake
// scheduler.Scheduler via schedulerFactoryFn so we never touch the real
// `\mcp-local-hub-supervisor` Task Scheduler entry the developer might
// have installed locally. t.Cleanup restores the seam between tests.
package autostart

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// fakeScheduler is a recording Scheduler implementing every Backend
// method the autostart Windows code calls. Tests inspect .createCalls
// / .deleteCalls / .statusReturn / .xmlReturn to verify the contract.
//
// Only Create, Delete, Status, ExportXML are exercised by autostart's
// Windows path; the rest hard-fail to expose accidental call-site
// expansion.
type fakeScheduler struct {
	createCalls   []scheduler.TaskSpec
	deleteCalls   []string
	statusReturn  scheduler.TaskStatus
	statusErr     error
	xmlReturn     []byte
	xmlErr        error
	createErr     error
	deleteErr     error
	statusByName  map[string]scheduler.TaskStatus // optional override
	xmlByName     map[string][]byte               // optional override
	statusErrByName map[string]error
	xmlErrByName    map[string]error
}

func (f *fakeScheduler) Create(spec scheduler.TaskSpec) error {
	f.createCalls = append(f.createCalls, spec)
	return f.createErr
}

func (f *fakeScheduler) Delete(name string) error {
	f.deleteCalls = append(f.deleteCalls, name)
	return f.deleteErr
}

func (f *fakeScheduler) Status(name string) (scheduler.TaskStatus, error) {
	if err, ok := f.statusErrByName[name]; ok {
		return scheduler.TaskStatus{}, err
	}
	if s, ok := f.statusByName[name]; ok {
		return s, nil
	}
	return f.statusReturn, f.statusErr
}

func (f *fakeScheduler) ExportXML(name string) ([]byte, error) {
	if err, ok := f.xmlErrByName[name]; ok {
		return nil, err
	}
	if x, ok := f.xmlByName[name]; ok {
		return x, nil
	}
	return f.xmlReturn, f.xmlErr
}

// Unused methods — hard-fail if autostart calls them.
func (f *fakeScheduler) Run(string) error             { panic("fakeScheduler: Run unexpected") }
func (f *fakeScheduler) Stop(string) error            { panic("fakeScheduler: Stop unexpected") }
func (f *fakeScheduler) List(string) ([]scheduler.TaskStatus, error) {
	panic("fakeScheduler: List unexpected")
}
func (f *fakeScheduler) ImportXML(string, []byte) error {
	panic("fakeScheduler: ImportXML unexpected")
}

// withFakeScheduler installs a fake scheduler factory for the duration
// of one test and restores the prior factory on cleanup.
func withFakeScheduler(t *testing.T, f *fakeScheduler) {
	t.Helper()
	prev := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return f, nil }
	t.Cleanup(func() { schedulerFactoryFn = prev })
}

func TestWindowsBackend_EnableCreatesTask(t *testing.T) {
	f := &fakeScheduler{}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: `C:\mcp\mcphub.exe`}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(f.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1: %+v", len(f.createCalls), f.createCalls)
	}
	spec := f.createCalls[0]
	if spec.Name != WindowsTaskName {
		t.Errorf("Create.Name = %q, want %q", spec.Name, WindowsTaskName)
	}
	if !spec.LogonTrigger {
		t.Error("Create.LogonTrigger = false, want true")
	}
	if spec.Command != `C:\mcp\mcphub.exe` {
		t.Errorf("Create.Command = %q, want %q", spec.Command, `C:\mcp\mcphub.exe`)
	}
	// As of 2026-05-18, the autostart entry launches `mcphub gui`
	// instead of `mcphub supervise` — GUI process owns supervisor
	// lifecycle (see internal/cli/gui_supervisor_owner.go).
	if len(spec.Args) != 1 || spec.Args[0] != "gui" {
		t.Errorf("Create.Args = %v, want [gui]", spec.Args)
	}
}

func TestWindowsBackend_EnableStrictModeAddsFlag(t *testing.T) {
	f := &fakeScheduler{}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{StrictMode: true, MCPHubPath: `C:\mcp\mcphub.exe`}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(f.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1", len(f.createCalls))
	}
	got := f.createCalls[0].Args
	// As of 2026-05-18, autostart launches `mcphub gui --strict-mode`
	// (GUI owns supervisor lifecycle; --strict-mode threads through).
	want := []string{"gui", "--strict-mode"}
	if len(got) != len(want) {
		t.Fatalf("Create.Args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Create.Args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWindowsBackend_EnableReplacesExistingTask(t *testing.T) {
	// Pre-existing task → Enable must Delete-then-Create to keep the
	// replacement atomic vs scheduler.Create's "already exists" error.
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Ready"},
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: `C:\mcp\mcphub.exe`}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	// As of 2026-05-18, Enable also deletes the legacy v0.4.x watchdog
	// task as part of the v0.5.0 cleanup. Expect both Delete calls in
	// the recorded order: supervisor first (pre-Create idempotence),
	// watchdog second (post-Create cleanup).
	const legacyWatchdogTaskName = `\mcp-local-hub-watchdog`
	wantDeletes := []string{WindowsTaskName, legacyWatchdogTaskName}
	if len(f.deleteCalls) != len(wantDeletes) {
		t.Fatalf("Delete calls = %v, want %v", f.deleteCalls, wantDeletes)
	}
	for i, want := range wantDeletes {
		if f.deleteCalls[i] != want {
			t.Errorf("Delete[%d] = %q, want %q", i, f.deleteCalls[i], want)
		}
	}
	if len(f.createCalls) != 1 {
		t.Errorf("Create calls = %d, want 1", len(f.createCalls))
	}
}

func TestWindowsBackend_DisableDeletesTask(t *testing.T) {
	f := &fakeScheduler{}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if len(f.deleteCalls) != 1 || f.deleteCalls[0] != WindowsTaskName {
		t.Errorf("Delete calls = %v, want [%q]", f.deleteCalls, WindowsTaskName)
	}
}

func TestWindowsBackend_DisableIdempotent(t *testing.T) {
	// scheduler.Delete is idempotent (returns nil when absent). Verify
	// our backend propagates that — Disable on a clean system returns
	// nil, not an error.
	f := &fakeScheduler{deleteErr: nil}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := b.Disable(); err != nil {
			t.Fatalf("Disable iter %d: %v", i, err)
		}
	}
	if len(f.deleteCalls) != 3 {
		t.Errorf("Delete calls = %d, want 3 (idempotent re-invocations)", len(f.deleteCalls))
	}
}

func TestWindowsBackend_StatusAbsent(t *testing.T) {
	f := &fakeScheduler{statusErr: scheduler.ErrTaskNotFound}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `C:\mcp\mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateAbsent {
		t.Errorf("Status = %s, want %s", got, StateAbsent)
	}
}

func TestWindowsBackend_StatusEnabledRunning(t *testing.T) {
	xml := buildMatchingXML(`C:\mcp\mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `C:\mcp\mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateEnabledRunning {
		t.Errorf("Status = %s, want %s", got, StateEnabledRunning)
	}
}

func TestWindowsBackend_StatusEnabledStopped(t *testing.T) {
	xml := buildMatchingXML(`C:\mcp\mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Ready"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `C:\mcp\mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateEnabledStopped {
		t.Errorf("Status = %s, want %s", got, StateEnabledStopped)
	}
}

func TestWindowsBackend_StatusDrifted_StrictModeMissing(t *testing.T) {
	// Recorded XML has supervise WITHOUT --strict-mode; caller asks for
	// strict-mode → drift.
	xml := buildMatchingXML(`C:\mcp\mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{StrictMode: true, MCPHubPath: `C:\mcp\mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (strict-mode flag missing from XML)", got, StateDrifted)
	}
}

func TestWindowsBackend_StatusDrifted_StrictModeUnwanted(t *testing.T) {
	// Recorded XML has supervise --strict-mode; caller asks WITHOUT
	// strict-mode → drift.
	xml := buildMatchingXML(`C:\mcp\mcphub.exe`, true)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{StrictMode: false, MCPHubPath: `C:\mcp\mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (strict-mode flag present unexpectedly)", got, StateDrifted)
	}
}

func TestWindowsBackend_StatusDrifted_CommandPath(t *testing.T) {
	// Recorded XML points at an older binary path; caller's MCPHubPath
	// disagrees → drift.
	xml := buildMatchingXML(`C:\old\mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `C:\new\mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (binary path mismatch)", got, StateDrifted)
	}
}

// TestWindowsBackend_StatusXMLUnavailable_TreatedAsRunning verifies the
// fallback path: when ExportXML returns ErrTaskNotFound mid-flight (a
// concurrent delete or a transient query failure), we still report
// running state from the Status call instead of error-ing out. Drift
// detection is best-effort.
func TestWindowsBackend_StatusXMLUnavailable_TreatedAsRunning(t *testing.T) {
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlErr:       scheduler.ErrTaskNotFound,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `C:\mcp\mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateEnabledRunning {
		t.Errorf("Status = %s, want %s (XML transient absent → keep state)", got, StateEnabledRunning)
	}
}

func TestWindowsBackend_StatusSchedulerFactoryError(t *testing.T) {
	// Factory failure surfaces as Status error; the backend cannot
	// distinguish absent from broken without a scheduler handle.
	prev := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		return nil, errors.New("scheduler.New: COM init failed")
	}
	t.Cleanup(func() { schedulerFactoryFn = prev })

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Status(Options{MCPHubPath: `C:\mcp\mcphub.exe`})
	if err == nil {
		t.Fatal("Status returned nil err, want propagated scheduler-factory failure")
	}
	if !strings.Contains(err.Error(), "scheduler") {
		t.Errorf("Status err = %v; want a scheduler-related error", err)
	}
}

// buildMatchingXML produces the minimal Task Scheduler XML fragment
// our drift detector parses — just <Command> and <Arguments>. The
// real schtasks /Query /XML output is larger, but the detector only
// inspects these two fields.
func buildMatchingXML(command string, strictMode bool) []byte {
	args := "supervise"
	if strictMode {
		args = "supervise --strict-mode"
	}
	return []byte(fmt.Sprintf(
		"<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n"+
			"<Task><Actions><Exec>\n"+
			"  <Command>%s</Command>\n"+
			"  <Arguments>%s</Arguments>\n"+
			"</Exec></Actions></Task>\n",
		command, args,
	))
}
