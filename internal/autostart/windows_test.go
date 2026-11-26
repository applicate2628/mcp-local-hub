//go:build windows

// Windows autostart backend tests. Every test injects a fake
// scheduler.Scheduler via schedulerFactoryFn so we never touch the real
// `\mcp-local-hub-supervisor` Task Scheduler entry the developer might
// have installed locally. t.Cleanup restores the seam between tests.
package autostart

import (
	"bytes"
	"errors"
	"fmt"
	"os/user"
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
	createCalls     []scheduler.TaskSpec
	deleteCalls     []string
	exportCalls     []string
	statusCalls     []string
	exportReplies   []testTaskExport
	statusReturn    scheduler.TaskStatus
	statusErr       error
	xmlReturn       []byte
	xmlErr          error
	createErr       error
	deleteErr       error
	statusByName    map[string]scheduler.TaskStatus // optional override
	xmlByName       map[string][]byte               // optional override
	statusErrByName map[string]error
	xmlErrByName    map[string]error
}

type testTaskExport struct {
	xml []byte
	err error
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
	f.statusCalls = append(f.statusCalls, name)
	if err, ok := f.statusErrByName[name]; ok {
		return scheduler.TaskStatus{}, err
	}
	if s, ok := f.statusByName[name]; ok {
		return s, nil
	}
	return f.statusReturn, f.statusErr
}

func (f *fakeScheduler) ExportXML(name string) ([]byte, error) {
	f.exportCalls = append(f.exportCalls, name)
	if len(f.exportReplies) != 0 {
		reply := f.exportReplies[0]
		f.exportReplies = f.exportReplies[1:]
		return reply.xml, reply.err
	}
	if err, ok := f.xmlErrByName[name]; ok {
		return nil, err
	}
	if x, ok := f.xmlByName[name]; ok {
		return x, nil
	}
	return f.xmlReturn, f.xmlErr
}

// Unused methods — hard-fail if autostart calls them.
func (f *fakeScheduler) Run(string) error  { panic("fakeScheduler: Run unexpected") }
func (f *fakeScheduler) Stop(string) error { panic("fakeScheduler: Stop unexpected") }
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
	const legacyWatchdog = `\mcp-local-hub-watchdog`
	f := &fakeScheduler{
		xmlErr:    scheduler.ErrTaskNotFound,
		xmlByName: map[string][]byte{legacyWatchdog: []byte("<Task/>")},
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: `fixture-mcphub.exe`}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(f.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1: %+v", len(f.createCalls), f.createCalls)
	}
	if len(f.deleteCalls) != 0 || len(f.exportCalls) != 1 || f.exportCalls[0] != WindowsTaskName {
		t.Fatalf("creation touched another task or attempted deletion: export=%v delete=%v", f.exportCalls, f.deleteCalls)
	}
	spec := f.createCalls[0]
	if spec.Name != WindowsTaskName {
		t.Errorf("Create.Name = %q, want %q", spec.Name, WindowsTaskName)
	}
	if !spec.LogonTrigger {
		t.Error("Create.LogonTrigger = false, want true")
	}
	if spec.Command != `fixture-mcphub.exe` {
		t.Errorf("Create.Command = %q, want %q", spec.Command, `fixture-mcphub.exe`)
	}
	// As of 2026-05-18, the autostart entry launches `mcphub gui`
	// instead of `mcphub supervise` — GUI process owns supervisor
	// lifecycle (see internal/cli/gui_supervisor_owner.go).
	if len(spec.Args) != 2 || spec.Args[0] != "gui" || spec.Args[1] != "--no-browser" {
		t.Errorf("Create.Args = %v, want [gui --no-browser]", spec.Args)
	}
}

func TestWindowsBackend_DefaultExecutableRoutesOwnedLogonThroughCanonicalProduct(t *testing.T) {
	prev := osExecutableFn
	osExecutableFn = func() (string, error) { return `C:\bin\mcphub.exe`, nil }
	t.Cleanup(func() { osExecutableFn = prev })
	f := &fakeScheduler{xmlErr: scheduler.ErrTaskNotFound}
	withFakeScheduler(t, f)
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Enable(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := f.createCalls[0].Command; got != `C:\bin\mcphub.exe` {
		t.Fatalf("Command=%q", got)
	}
}

func TestWindowsBackend_EnableStrictModeAddsFlag(t *testing.T) {
	f := &fakeScheduler{xmlErr: scheduler.ErrTaskNotFound}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{StrictMode: true, MCPHubPath: `fixture-mcphub.exe`}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(f.createCalls) != 1 {
		t.Fatalf("Create called %d times, want 1", len(f.createCalls))
	}
	got := f.createCalls[0].Args
	// As of 2026-05-18, autostart launches `mcphub gui --strict-mode`
	// (GUI owns supervisor lifecycle; --strict-mode threads through).
	want := []string{"gui", "--no-browser", "--strict-mode"}
	if len(got) != len(want) {
		t.Fatalf("Create.Args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Create.Args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWindowsBackend_EnableOwnerModeSelectsExactlyOneOwnerCommand(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want []string
	}{
		{name: "gui non-strict", opts: Options{OwnerMode: OwnerModeGUI, MCPHubPath: `fixture-mcphub.exe`}, want: []string{"gui", "--no-browser"}},
		{name: "gui strict", opts: Options{OwnerMode: OwnerModeGUI, StrictMode: true, MCPHubPath: `fixture-mcphub.exe`}, want: []string{"gui", "--no-browser", "--strict-mode"}},
		{name: "supervise non-strict", opts: Options{OwnerMode: OwnerModeSupervise, MCPHubPath: `fixture-mcphub.exe`}, want: []string{"supervise"}},
		{name: "supervise strict", opts: Options{OwnerMode: OwnerModeSupervise, StrictMode: true, MCPHubPath: `fixture-mcphub.exe`}, want: []string{"supervise", "--strict-mode"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{xmlErr: scheduler.ErrTaskNotFound}
			withFakeScheduler(t, f)
			b, err := New()
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := b.Enable(tc.opts); err != nil {
				t.Fatalf("Enable: %v", err)
			}
			if len(f.createCalls) != 1 {
				t.Fatalf("Create calls=%d, want 1", len(f.createCalls))
			}
			got := f.createCalls[0].Args
			if len(got) != len(tc.want) {
				t.Fatalf("Create.Args=%v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("Create.Args=%v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestWindowsBackend_EnableRejectsInvalidOwnerBeforeMutatingTask(t *testing.T) {
	f := &fakeScheduler{}
	withFakeScheduler(t, f)
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = b.Enable(Options{OwnerMode: OwnerMode("invalid"), MCPHubPath: "fixture-mcphub.exe"})
	if err == nil {
		t.Fatal("Enable returned nil for invalid owner mode")
	}
	if len(f.deleteCalls) != 0 || len(f.createCalls) != 0 || len(f.exportCalls) != 0 {
		t.Fatalf("invalid mode reached scheduler: export=%v delete=%v create=%+v", f.exportCalls, f.deleteCalls, f.createCalls)
	}
}

func TestWindowsBackend_EnableExistingTaskIsReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  Options
		args  string
		state scheduler.TaskRuntimeState
	}{
		{name: "gui_ready", opts: Options{OwnerMode: OwnerModeGUI, MCPHubPath: "fixture-mcphub.exe"}, args: "gui --no-browser", state: scheduler.TaskRuntimeReady},
		{name: "supervise_running", opts: Options{OwnerMode: OwnerModeSupervise, StrictMode: true, MCPHubPath: "fixture-mcphub.exe"}, args: "supervise --strict-mode", state: scheduler.TaskRuntimeRunning},
		{name: "gui_queued", opts: Options{MCPHubPath: "fixture-mcphub.exe"}, args: "gui --no-browser", state: scheduler.TaskRuntimeQueued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := buildEnabledAutostartXML(t, tc.opts.MCPHubPath, tc.args)
			f := &fakeScheduler{xmlReturn: bytes.Clone(original), statusReturn: scheduler.TaskStatus{RuntimeState: tc.state}}
			withFakeScheduler(t, f)
			for range 2 {
				if err := (&windowsBackend{}).Enable(tc.opts); err != nil {
					t.Fatalf("repeat Enable: %v", err)
				}
			}
			if len(f.createCalls) != 0 || len(f.deleteCalls) != 0 || len(f.exportCalls) != 2 || len(f.statusCalls) != 2 || !bytes.Equal(f.xmlReturn, original) {
				t.Fatalf("existing task was changed: create=%v delete=%v export=%v status=%v", f.createCalls, f.deleteCalls, f.exportCalls, f.statusCalls)
			}
			// Run, Stop, and ImportXML panic in this fake if ever called.
		})
	}
}

func TestWindowsBackend_EnableRetainsDifferentOrDisabledDefinition(t *testing.T) {
	opts := Options{MCPHubPath: "fixture-mcphub.exe"}
	canonical := buildEnabledAutostartXML(t, opts.MCPHubPath, "gui --no-browser")
	for _, tc := range []struct {
		name string
		opts Options
		xml  []byte
	}{
		{name: "strict_policy_changed", opts: Options{MCPHubPath: opts.MCPHubPath, StrictMode: true}, xml: canonical},
		{name: "owner_mode_changed", opts: Options{MCPHubPath: opts.MCPHubPath, OwnerMode: OwnerModeSupervise}, xml: canonical},
		{name: "disabled_definition", opts: opts, xml: []byte(strings.Replace(string(canonical), "<Settings>", "<Settings><Enabled>false</Enabled>", 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{xmlReturn: bytes.Clone(tc.xml), statusReturn: scheduler.TaskStatus{RuntimeState: scheduler.TaskRuntimeReady}}
			withFakeScheduler(t, f)
			err := (&windowsBackend{}).Enable(tc.opts)
			refusal, ok := err.(*ObservedTaskReplacementRefusal)
			if !ok || !errors.Is(err, scheduler.ErrTaskReplacementUnowned) || !bytes.Equal(refusal.ObservedXML(), tc.xml) {
				t.Fatalf("definition was not retained: %v", err)
			}
			if len(f.createCalls) != 0 || len(f.deleteCalls) != 0 || len(f.statusCalls) != 0 || !bytes.Equal(f.xmlReturn, tc.xml) {
				t.Fatalf("different task was changed or accepted: create=%v delete=%v status=%v", f.createCalls, f.deleteCalls, f.statusCalls)
			}
		})
	}
}

func TestWindowsBackend_EnableRequiresObservedEnabledRuntime(t *testing.T) {
	opts := Options{MCPHubPath: "fixture-mcphub.exe"}
	canonical := buildEnabledAutostartXML(t, opts.MCPHubPath, "gui --no-browser")
	for _, tc := range []struct {
		name   string
		status scheduler.TaskStatus
		err    error
		reason string
	}{
		{name: "disabled", status: scheduler.TaskStatus{RuntimeState: scheduler.TaskRuntimeDisabled}, reason: "disabled"},
		{name: "disabled_display", status: scheduler.TaskStatus{RuntimeState: scheduler.TaskRuntimeReady, State: "Disabled"}, reason: "disabled"},
		{name: "unknown", status: scheduler.TaskStatus{RuntimeState: scheduler.TaskRuntimeUnknown, State: "Ready"}, reason: "unknown"},
		{name: "disappeared", err: scheduler.ErrTaskNotFound, reason: "observe existing autostart runtime state"},
		{name: "unavailable", err: scheduler.ErrUnavailable, reason: "observe existing autostart runtime state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{xmlReturn: bytes.Clone(canonical), statusReturn: tc.status, statusErr: tc.err}
			withFakeScheduler(t, f)
			err := (&windowsBackend{}).Enable(opts)
			refusal, ok := err.(*ObservedTaskReplacementRefusal)
			if !ok || !errors.Is(err, scheduler.ErrTaskReplacementUnowned) || !bytes.Equal(refusal.ObservedXML(), canonical) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("runtime uncertainty was not retained: %v", err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("runtime observation cause lost: %v", err)
			}
			if len(f.createCalls) != 0 || len(f.deleteCalls) != 0 || len(f.exportCalls) != 1 || len(f.statusCalls) != 1 || !bytes.Equal(f.xmlReturn, canonical) {
				t.Fatalf("runtime refusal changed the task: create=%v delete=%v export=%v status=%v", f.createCalls, f.deleteCalls, f.exportCalls, f.statusCalls)
			}
		})
	}
}

func TestWindowsBackend_EnableRefusesExistingIncompleteTask(t *testing.T) {
	opts := Options{MCPHubPath: `fixture-mcphub.exe`}
	for _, tc := range []struct {
		name string
		xml  []byte
	}{
		{name: "matching_visible_argv", xml: buildMatchingXML(opts.MCPHubPath, false)},
		{name: "different_definition", xml: scheduler.EncodeXMLUTF16LEBOM("<Task>observed-canary</Task>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{xmlReturn: bytes.Clone(tc.xml)}
			withFakeScheduler(t, f)
			b, err := New()
			if err != nil {
				t.Fatal(err)
			}
			err = b.Enable(opts)
			refusal, ok := err.(*ObservedTaskReplacementRefusal)
			if !ok || !errors.Is(err, scheduler.ErrTaskReplacementUnowned) || refusal.TaskName != WindowsTaskName {
				t.Fatalf("Enable error = %v, want a named generation refusal", err)
			}
			if !errors.Is(err, scheduler.ErrTaskCorrupt) {
				t.Fatalf("definition observation cause lost: %v", err)
			}
			refusal.ObservedXML()[0] ^= 0xff
			f.xmlReturn[0] ^= 0xff
			if !bytes.Equal(refusal.ObservedXML(), tc.xml) || strings.Contains(err.Error(), string(tc.xml)) ||
				strings.Contains(fmt.Sprintf("%#v", err), string(tc.xml)) ||
				fmt.Sprintf("%#v", err) != err.Error() {
				t.Fatal("refusal changed or exposed the captured definition")
			}
			if len(f.createCalls) != 0 || len(f.deleteCalls) != 0 || len(f.exportCalls) != 1 {
				t.Fatalf("occupied task was mutated: export=%v create=%v delete=%v", f.exportCalls, f.createCalls, f.deleteCalls)
			}
		})
	}
}

func TestWindowsBackend_EnableRequiresConfirmedAbsence(t *testing.T) {
	for _, tc := range []struct {
		name string
		testTaskExport
		want error
	}{
		{name: "conflicting_absence", testTaskExport: testTaskExport{[]byte("<Task/>"), scheduler.ErrTaskNotFound}, want: scheduler.ErrTaskNotFound},
		{name: "partial_read", testTaskExport: testTaskExport{[]byte("<Task>"), scheduler.ErrPermissionDenied}, want: scheduler.ErrPermissionDenied},
		{name: "unavailable", testTaskExport: testTaskExport{nil, scheduler.ErrUnavailable}, want: scheduler.ErrUnavailable},
		{name: "empty_success", want: scheduler.ErrTaskCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{xmlReturn: tc.xml, xmlErr: tc.err}
			withFakeScheduler(t, f)
			err := (&windowsBackend{}).Enable(Options{MCPHubPath: `fixture-mcphub.exe`})
			if !errors.Is(err, tc.want) || len(f.createCalls) != 0 || len(f.deleteCalls) != 0 {
				t.Fatalf("unsafe absence: err=%v create=%v delete=%v", err, f.createCalls, f.deleteCalls)
			}
		})
	}
}

func TestWindowsBackend_EnableAfterCreateFailureOnlyObserves(t *testing.T) {
	prior := []byte("<Task>external occupant</Task>")
	for _, tc := range []struct {
		name      string
		createErr error
		after     testTaskExport
	}{
		{name: "conflict", createErr: scheduler.ErrTaskAlreadyExists, after: testTaskExport{xml: prior}},
		{name: "matching_conflict", createErr: scheduler.ErrTaskAlreadyExists, after: testTaskExport{xml: buildEnabledAutostartXML(t, "fixture-mcphub.exe", "gui --no-browser")}},
		{name: "lost_acknowledgment", createErr: scheduler.ErrMutationOutcomeUnknown, after: testTaskExport{xml: prior}},
		{name: "absent", createErr: scheduler.ErrPermissionDenied, after: testTaskExport{err: scheduler.ErrTaskNotFound}},
		{name: "read_unavailable", createErr: scheduler.ErrMutationOutcomeUnknown, after: testTaskExport{err: scheduler.ErrUnavailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{
				createErr: tc.createErr,
				exportReplies: []testTaskExport{
					{err: scheduler.ErrTaskNotFound},
					tc.after,
				},
			}
			withFakeScheduler(t, f)
			err := (&windowsBackend{}).Enable(Options{MCPHubPath: `fixture-mcphub.exe`})
			if !errors.Is(err, tc.createErr) || len(f.createCalls) != 1 || len(f.deleteCalls) != 0 || len(f.exportCalls) != 2 {
				t.Fatalf("failed creation was retried or compensated: err=%v create=%v delete=%v export=%v", err, f.createCalls, f.deleteCalls, f.exportCalls)
			}
			var refusal *ObservedTaskReplacementRefusal
			if len(tc.after.xml) != 0 {
				if !errors.As(err, &refusal) || !bytes.Equal(refusal.ObservedXML(), tc.after.xml) {
					t.Fatalf("post-create observation missing from %v", err)
				}
			} else if errors.As(err, &refusal) {
				t.Fatalf("unexpected retention receipt: %v", err)
			}
			if errors.Is(tc.after.err, scheduler.ErrUnavailable) && !errors.Is(err, scheduler.ErrUnavailable) {
				t.Fatalf("post-create observation cause lost: %v", err)
			}
		})
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
	got, err := b.Status(Options{MCPHubPath: `fixture-mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateAbsent {
		t.Errorf("Status = %s, want %s", got, StateAbsent)
	}
}

func TestWindowsBackend_StatusEnabledRunning(t *testing.T) {
	xml := buildMatchingXML(`fixture-mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `fixture-mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateEnabledRunning {
		t.Errorf("Status = %s, want %s", got, StateEnabledRunning)
	}
}

func TestWindowsBackend_StatusEnabledStopped(t *testing.T) {
	xml := buildMatchingXML(`fixture-mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Ready"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `fixture-mcphub.exe`})
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
	xml := buildMatchingXML(`fixture-mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{StrictMode: true, MCPHubPath: `fixture-mcphub.exe`})
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
	xml := buildMatchingXML(`fixture-mcphub.exe`, true)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{StrictMode: false, MCPHubPath: `fixture-mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (strict-mode flag present unexpectedly)", got, StateDrifted)
	}
}

// TestWindowsBackend_StatusDrifted_LegacySupervisorSubcommand asserts
// that a pre-PR #212 autostart entry installed with
// `<Arguments>supervise</Arguments>` is reported as drifted under
// the post-PR #212 default (Args=["gui"]). Without the subcommand
// drift check in detectDrift, the legacy install would silently
// satisfy `enabled-running` and the operator would miss the cue to
// re-run `mcphub autostart enable`. PR #212 r4 finding 2.
func TestWindowsBackend_StatusDrifted_LegacySupervisorSubcommand(t *testing.T) {
	const legacySuperviseXML = "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n" +
		"<Task><Actions><Exec>\n" +
		"  <Command>fixture-mcphub.exe</Command>\n" +
		"  <Arguments>supervise</Arguments>\n" +
		"</Exec></Actions></Task>\n"
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    []byte(legacySuperviseXML),
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `fixture-mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (Args=[supervise] should be drifted under Args=[gui] default)", got, StateDrifted)
	}
}

func TestWindowsBackend_StatusDrifted_WhenOwnerModeDoesNotMatchTask(t *testing.T) {
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    buildWindowsStatusSnapshotXML(`fixture-mcphub.exe`, "gui --no-browser", "true", ""),
	}
	withFakeScheduler(t, f)
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state, err := b.Status(Options{OwnerMode: OwnerModeSupervise, MCPHubPath: `fixture-mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state != StateDrifted {
		t.Fatalf("Status=%s, want drifted when GUI task is not the persisted supervise owner", state)
	}
}

func TestWindowsTaskSpecDrifted_RequiresExactCanonicalOwnerArgv(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		args string
		want bool
	}{
		{name: "gui canonical", opts: Options{OwnerMode: OwnerModeGUI, MCPHubPath: "fixture-mcphub.exe"}, args: "gui --no-browser", want: false},
		{name: "gui missing no-browser", opts: Options{OwnerMode: OwnerModeGUI, MCPHubPath: "fixture-mcphub.exe"}, args: "gui", want: true},
		{name: "gui extra token", opts: Options{OwnerMode: OwnerModeGUI, MCPHubPath: "fixture-mcphub.exe"}, args: "gui --no-browser --bad", want: true},
		{name: "gui duplicate strict", opts: Options{OwnerMode: OwnerModeGUI, StrictMode: true, MCPHubPath: "fixture-mcphub.exe"}, args: "gui --no-browser --strict-mode --strict-mode", want: true},
		{name: "supervise canonical", opts: Options{OwnerMode: OwnerModeSupervise, StrictMode: true, MCPHubPath: "fixture-mcphub.exe"}, args: "supervise --strict-mode", want: false},
		{name: "supervise must not carry no-browser", opts: Options{OwnerMode: OwnerModeSupervise, MCPHubPath: "fixture-mcphub.exe"}, args: "supervise --no-browser", want: true},
		{name: "supervise extra token", opts: Options{OwnerMode: OwnerModeSupervise, MCPHubPath: "fixture-mcphub.exe"}, args: "supervise --bad", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := windowsTaskSpecDrifted(windowsTaskSpec{Command: "fixture-mcphub.exe", Arguments: tc.args}, tc.opts)
			if got != tc.want {
				t.Fatalf("windowsTaskSpecDrifted(args=%q)=%v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestWindowsBackend_StatusDrifted_EmptyArguments locks down the
// empty-Arguments edge case identified by QA-r6 Gap 3: a recorded
// XML whose <Arguments> tag contains an empty string must be
// classified as drifted (operator's prior call wrote zero args,
// post-PR #212 expectation is ["gui"]). Without this assertion a
// future change that loosens detectDrift to treat empty as "no
// drift" would silently accept a broken autostart entry.
func TestWindowsBackend_StatusDrifted_EmptyArguments(t *testing.T) {
	const emptyArgsXML = "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n" +
		"<Task><Actions><Exec>\n" +
		"  <Command>fixture-mcphub.exe</Command>\n" +
		"  <Arguments></Arguments>\n" +
		"</Exec></Actions></Task>\n"
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    []byte(emptyArgsXML),
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `fixture-mcphub.exe`})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (empty Arguments must drift against Args=[gui] default)", got, StateDrifted)
	}
}

func TestWindowsBackend_StatusDrifted_CommandPath(t *testing.T) {
	// Recorded XML points at an older binary path; caller's MCPHubPath
	// disagrees → drift.
	xml := buildMatchingXML(`fixture-old-mcphub.exe`, false)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    xml,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: `fixture-new-mcphub.exe`})
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
	got, err := b.Status(Options{MCPHubPath: `fixture-mcphub.exe`})
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
	_, err = b.Status(Options{MCPHubPath: `fixture-mcphub.exe`})
	if err == nil {
		t.Fatal("Status returned nil err, want propagated scheduler-factory failure")
	}
	if !strings.Contains(err.Error(), "scheduler") {
		t.Errorf("Status err = %v; want a scheduler-related error", err)
	}
}

func TestWindowsBackend_StatusSnapshotXMLUnavailableFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		xml    []byte
		xmlErr error
	}{
		{name: "export-error", xmlErr: errors.New("schtasks export failed")},
		{name: "empty-xml", xml: nil},
		{name: "malformed-xml", xml: []byte("<Task>")},
		{name: "disappeared-after-status", xmlErr: scheduler.ErrTaskNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{
				statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
				xmlReturn:    tc.xml,
				xmlErr:       tc.xmlErr,
			}
			withFakeScheduler(t, f)

			b, err := New()
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			snapshot, err := b.StatusSnapshot(Options{MCPHubPath: `fixture-mcphub.exe`})
			if err == nil {
				t.Fatalf("StatusSnapshot returned nil err and snapshot=%+v; XML uncertainty must fail closed", snapshot)
			}
			if snapshot.SpecFingerprint != "" {
				t.Fatalf("StatusSnapshot returned fingerprint %q on unavailable XML, want no comparable spec", snapshot.SpecFingerprint)
			}
		})
	}
}

func TestWindowsBackend_StatusRequiresTypedAbsence(t *testing.T) {
	statusErr := errors.New("cannot find scheduler bridge")
	withFakeScheduler(t, &fakeScheduler{statusErr: statusErr})
	b := &windowsBackend{}
	opts := Options{MCPHubPath: `fixture-mcphub.exe`}
	if _, err := b.Status(opts); !errors.Is(err, statusErr) {
		t.Fatalf("Status counted an untyped lookup failure as absence: %v", err)
	}
	if _, err := b.StatusSnapshot(opts); !errors.Is(err, statusErr) {
		t.Fatalf("StatusSnapshot counted an untyped lookup failure as absence: %v", err)
	}
}

func TestWindowsBackend_ProjectAutorunDisabledRequiresTypedAbsence(t *testing.T) {
	readbackErr := errors.New("scheduler bridge executable does not exist")
	f := &fakeScheduler{statusErr: readbackErr}
	withFakeScheduler(t, f)
	b := &windowsBackend{}
	opts := Options{MCPHubPath: `fixture-mcphub.exe`}
	off := false
	if err := ProjectAutorunDisabled(&off, b, opts); !errors.Is(err, readbackErr) {
		t.Fatalf("OFF reported success without an absence readback: %v", err)
	}
	f.statusErr = scheduler.ErrTaskNotFound
	if err := ProjectAutorunDisabled(&off, b, opts); err != nil {
		t.Fatalf("confirmed absence did not settle OFF: %v", err)
	}
	if len(f.deleteCalls) != 2 || f.deleteCalls[0] != WindowsTaskName || f.deleteCalls[1] != WindowsTaskName {
		t.Fatalf("OFF did not request explicit removal of the canonical task: %v", f.deleteCalls)
	}
}

func TestWindowsBackend_StatusSnapshotFingerprintsCompleteDefinition(t *testing.T) {
	baseXML := buildWindowsStatusSnapshotXML(
		`fixture-mcphub.exe`,
		"gui --no-browser",
		"true",
		"<Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>"+
			"<Principals><Principal id=\"Author\"><UserId>fixture-user-a</UserId></Principal></Principals>",
	)
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: "Running"},
		xmlReturn:    baseXML,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := Options{MCPHubPath: `fixture-mcphub.exe`}
	first, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot first: %v", err)
	}
	if first.SpecFingerprint == "" {
		t.Fatal("StatusSnapshot SpecFingerprint is empty, want installed shim fingerprint")
	}
	second, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot second: %v", err)
	}
	if second.SpecFingerprint != first.SpecFingerprint {
		t.Fatalf("SpecFingerprint unstable across identical re-probes: first=%q second=%q", first.SpecFingerprint, second.SpecFingerprint)
	}

	if ready := mustWindowsStatusSnapshotFingerprint(t, baseXML, "Ready", opts); ready != first.SpecFingerprint {
		t.Fatal("ordinary runtime liveness changed the definition fingerprint")
	}
	if disabled := mustWindowsStatusSnapshotFingerprint(t, baseXML, "Disabled", opts); disabled == first.SpecFingerprint {
		t.Fatal("runtime disabled state did not change the fingerprint")
	}
	for _, tc := range []struct {
		name, old, new string
	}{
		{name: "arguments", old: "gui --no-browser", new: "supervise"},
		{name: "enabled", old: "<Settings><Enabled>true", new: "<Settings><Enabled>false"},
		{name: "trigger", old: "<LogonTrigger>", new: "<LogonTrigger><Delay>PT1M</Delay>"},
		{name: "principal", old: "fixture-user-a", new: "fixture-user-b"},
		{name: "other_action", old: "</Actions>", new: "<Exec><Command>another.exe</Command></Exec></Actions>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := []byte(strings.Replace(string(baseXML), tc.old, tc.new, 1))
			if got := mustWindowsStatusSnapshotFingerprint(t, changed, "Running", opts); got == first.SpecFingerprint {
				t.Fatalf("fingerprint omitted the %s change", tc.name)
			}
		})
	}
}

// buildMatchingXML produces the minimal Task Scheduler XML fragment
// our drift detector parses — just <Command> and <Arguments>. The
// real schtasks /Query /XML output is larger, but the detector only
// inspects these two fields.
//
// Emits `gui --no-browser` / `gui --no-browser --strict-mode` to match the
// current autostart contract (PR #212: GUI owns supervisor lifecycle). Tests that
// deliberately need a pre-PR #212 `supervise` baseline (e.g. to
// exercise the subcommand-drift detection path) construct the XML
// inline rather than threading a third parameter through this helper.
func buildMatchingXML(command string, strictMode bool) []byte {
	args := "gui --no-browser"
	if strictMode {
		args = "gui --no-browser --strict-mode"
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

// buildEnabledAutostartXML is a native-style readback with default-valued fields
// omitted. The scheduler tests separately compare the actual Create XML writer.
func buildEnabledAutostartXML(t *testing.T, command, args string) []byte {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return []byte(fmt.Sprintf(`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
<Triggers><LogonTrigger><UserId>%s</UserId></LogonTrigger></Triggers>
<Principals><Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType></Principal></Principals>
<Settings><MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd></IdleSettings><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
<Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments></Exec></Actions>
</Task>`, current.Uid, current.Uid, command, args))
}

func buildWindowsStatusSnapshotXML(command, args, enabled, volatileBody string) []byte {
	return []byte(fmt.Sprintf(
		"<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n"+
			"<Task>"+
			"%s"+
			"<Settings><Enabled>%s</Enabled></Settings>"+
			"<Actions><Exec>"+
			"<Command>%s</Command>"+
			"<Arguments>%s</Arguments>"+
			"</Exec></Actions>"+
			"</Task>\n",
		volatileBody, enabled, command, args,
	))
}

func mustWindowsStatusSnapshotFingerprint(t *testing.T, xmlBlob []byte, state string, opts Options) string {
	t.Helper()
	f := &fakeScheduler{
		statusReturn: scheduler.TaskStatus{Name: WindowsTaskName, State: state},
		xmlReturn:    xmlBlob,
	}
	withFakeScheduler(t, f)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snapshot, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot: %v", err)
	}
	if snapshot.SpecFingerprint == "" {
		t.Fatal("StatusSnapshot SpecFingerprint is empty, want installed shim fingerprint")
	}
	return snapshot.SpecFingerprint
}
