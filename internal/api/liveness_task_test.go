package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"mcp-local-hub/internal/scheduler"
)

// installTestCanonicalMcphubPath overrides the canonical-mcphub-path resolver
// (canonicalMcphubPathFn, liveness_task.go) for the duration of the test. These
// two helpers were migrated here from the deleted watchdog_xml_validator_test.go
// when the v0.6 redesign removed the watchdog engine — the liveness-task tests
// are now their sole consumer.
func installTestCanonicalMcphubPath(t *testing.T, path string) {
	t.Helper()
	orig := canonicalMcphubPathFn
	canonicalMcphubPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { canonicalMcphubPathFn = orig })
}

type livenessTaskScheduler struct {
	tasks     map[string][]byte
	imports   []importXMLCall
	deletes   []string
	importErr error
}

func newLivenessTaskScheduler() *livenessTaskScheduler {
	return &livenessTaskScheduler{tasks: map[string][]byte{}}
}

func (f *livenessTaskScheduler) Create(scheduler.TaskSpec) error { return errNotImplementedForTest }
func (f *livenessTaskScheduler) Run(string) error                { return errNotImplementedForTest }
func (f *livenessTaskScheduler) Stop(string) error               { return errNotImplementedForTest }
func (f *livenessTaskScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, errNotImplementedForTest
}
func (f *livenessTaskScheduler) List(string) ([]scheduler.TaskStatus, error) {
	return nil, errNotImplementedForTest
}
func (f *livenessTaskScheduler) ExportXML(name string) ([]byte, error) {
	xml, ok := f.tasks[name]
	if !ok {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), xml...), nil
}
func (f *livenessTaskScheduler) ImportXML(name string, xml []byte) error {
	f.imports = append(f.imports, importXMLCall{name: name, xml: append([]byte(nil), xml...)})
	if f.importErr != nil {
		return f.importErr
	}
	f.tasks[name] = append([]byte(nil), xml...)
	return nil
}
func (f *livenessTaskScheduler) Delete(name string) error {
	f.deletes = append(f.deletes, name)
	delete(f.tasks, name)
	return nil
}
func (f *livenessTaskScheduler) importCalls() []importXMLCall {
	return append([]importXMLCall(nil), f.imports...)
}
func (f *livenessTaskScheduler) calls() []string { return append([]string(nil), f.deletes...) }

// installTestCurrentWindowsUser overrides the current-user resolver
// (currentWindowsUserFn, liveness_task.go) for the duration of the test.
func installTestCurrentWindowsUser(t *testing.T, name string) {
	t.Helper()
	orig := currentWindowsUserFn
	currentWindowsUserFn = func() (string, error) { return name, nil }
	t.Cleanup(func() { currentWindowsUserFn = orig })
}

// TestInstallLivenessTask_HappyPath asserts the supervisor-liveness install
// (v0.6 spec §15 P1-b / §5.x Phase 3a) resolves the canonical mcphub path +
// current user via the seams, then ImportXML under LivenessTaskName with the
// liveness XML body (PT1M cadence + `supervise --ensure-alive` action). Reuses
// the apiSurfacesFakeScheduler + seam helpers from api_surfaces_test.go.
func TestInstallLivenessTask_HappyPath(t *testing.T) {
	a := NewAPI()
	f := newLivenessTaskScheduler()
	installTestScheduler(t, f)
	installTestCanonicalMcphubPath(t, `C:\Users\test\.local\bin\mcphub.exe`)
	installTestCurrentWindowsUser(t, "test")

	if err := a.InstallLivenessTask(); err != nil {
		t.Fatalf("InstallLivenessTask: %v", err)
	}

	imports := f.importCalls()
	if len(imports) != 1 {
		t.Fatalf("expected 1 ImportXML call, got %d", len(imports))
	}
	if imports[0].name != LivenessTaskName {
		t.Errorf("ImportXML target name: got %q, want %q", imports[0].name, LivenessTaskName)
	}
	body := decodeUTF16LEBOMForTest(t, imports[0].xml)
	wantFragments := []string{
		"<Interval>PT1M</Interval>",
		"<ExecutionTimeLimit>PT1M</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<Arguments>supervise --ensure-alive</Arguments>",
		`<Command>C:\Users\test\.local\bin\mcphub.exe</Command>`,
		`<WorkingDirectory>C:\Users\test\.local\bin</WorkingDirectory>`,
		"<UserId>test</UserId>",
	}
	for _, w := range wantFragments {
		if !strings.Contains(body, w) {
			t.Errorf("ImportXML body missing %q; full body:\n%s", w, body)
		}
	}
	// The liveness install must NOT forward the watchdog action — proving it
	// is a distinct, additive task (the watchdog install is untouched).
	if strings.Contains(body, "watchdog --once") {
		t.Errorf("liveness ImportXML body unexpectedly contains the watchdog action")
	}
}

func TestInstallLivenessTask_ImportXMLReceivesUTF16LEBOM(t *testing.T) {
	a := NewAPI()
	f := newLivenessTaskScheduler()
	installTestScheduler(t, f)
	installTestCanonicalMcphubPath(t, `C:\Users\test\.local\bin\mcphub.exe`)
	installTestCurrentWindowsUser(t, "test")

	if err := a.InstallLivenessTask(); err != nil {
		t.Fatalf("InstallLivenessTask: %v", err)
	}
	imports := f.importCalls()
	if len(imports) != 1 {
		t.Fatalf("expected 1 ImportXML call, got %d", len(imports))
	}
	if len(imports[0].xml) < 2 || imports[0].xml[0] != 0xFF || imports[0].xml[1] != 0xFE {
		t.Fatalf("ImportXML bytes must start with UTF-16 LE BOM; first bytes=% x", imports[0].xml[:min(len(imports[0].xml), 8)])
	}
	decoded := decodeUTF16LEBOMForTest(t, imports[0].xml)
	if !strings.HasPrefix(decoded, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Fatalf("decoded liveness XML prefix = %q", decoded[:min(len(decoded), 80)])
	}
	if !strings.Contains(decoded, "<Arguments>supervise --ensure-alive</Arguments>") {
		t.Fatalf("decoded liveness XML missing ensure-alive action:\n%s", decoded)
	}
}

// TestInstallLivenessTask_Idempotent asserts a verified second run is a no-op.
func TestInstallLivenessTask_Idempotent(t *testing.T) {
	a := NewAPI()
	f := newLivenessTaskScheduler()
	installTestScheduler(t, f)
	installTestCanonicalMcphubPath(t, `C:\Users\test\.local\bin\mcphub.exe`)
	installTestCurrentWindowsUser(t, "test")

	if err := a.InstallLivenessTask(); err != nil {
		t.Fatalf("first InstallLivenessTask: %v", err)
	}
	if err := a.InstallLivenessTask(); err != nil {
		t.Fatalf("second InstallLivenessTask (idempotent): %v", err)
	}
	if got := len(f.importCalls()); got != 1 {
		t.Errorf("expected one initial ImportXML call, got %d", got)
	}
}

// TestInstallLivenessTask_PropagatesImportXMLError asserts a scheduler failure
// is surfaced verbatim — the install path does not swallow errors.
func TestInstallLivenessTask_PropagatesImportXMLError(t *testing.T) {
	a := NewAPI()
	want := errors.New("simulated schtasks failure")
	f := newLivenessTaskScheduler()
	f.importErr = want
	installTestScheduler(t, f)
	installTestCanonicalMcphubPath(t, `C:\Users\test\.local\bin\mcphub.exe`)
	installTestCurrentWindowsUser(t, "test")

	err := a.InstallLivenessTask()
	if err == nil {
		t.Fatal("InstallLivenessTask: want error, got nil")
	}
	if !errors.Is(err, want) {
		t.Errorf("InstallLivenessTask: want errors.Is(err, want); got %v", err)
	}
}

func TestLivenessTaskReceipt_RestoresExactPriorXMLAndPreservesForeignReplacement(t *testing.T) {
	a := NewAPI()
	f := newLivenessTaskScheduler()
	prior := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(
		`C:\Users\old\.local\bin\mcphub.exe`, `C:\Users\old\.local\bin`, "test"))
	f.tasks[LivenessTaskName] = prior
	installTestScheduler(t, f)
	installTestCanonicalMcphubPath(t, `C:\Users\test\.local\bin\mcphub.exe`)
	installTestCurrentWindowsUser(t, "test")

	receipt, err := a.EnsureLivenessTask()
	if err != nil {
		t.Fatalf("EnsureLivenessTask: %v", err)
	}
	if receipt.Result != LivenessTaskReplaced {
		t.Fatalf("receipt result = %q, want %q", receipt.Result, LivenessTaskReplaced)
	}
	if err := a.RestoreLivenessTask(receipt); err != nil {
		t.Fatalf("RestoreLivenessTask: %v", err)
	}
	if got := f.tasks[LivenessTaskName]; !bytes.Equal(got, prior) {
		t.Fatalf("SchedulerRollbackExactXML: restored XML = %q, want %q", got, prior)
	}

	receipt, err = a.EnsureLivenessTask()
	if err != nil {
		t.Fatalf("EnsureLivenessTask second: %v", err)
	}
	f.tasks[LivenessTaskName] = []byte("<Task>foreign</Task>")
	err = a.RestoreLivenessTask(receipt)
	if !errors.Is(err, ErrLivenessTaskRollbackConflict) {
		t.Fatalf("RestoreLivenessTask err = %v, want conflict", err)
	}
	if len(f.deletes) != 0 {
		t.Fatalf("foreign liveness task must not be deleted: %v", f.deletes)
	}
}

// TestLivenessWorkingDir_OSIndependent is the bot PR #288 F5 regression: the
// liveness <WorkingDirectory> derivation must NOT use path/filepath.Dir, which
// is OS-specific. On a non-Windows host filepath.Dir of a Windows-shaped path
// finds no '/' separator and returns "." — so the rendered XML (and the
// happy-path test's <WorkingDirectory> assertion) differed by host OS and the
// test FAILED on Linux/macOS. livenessWorkingDir splits on the last separator
// of EITHER kind, so it yields the correct parent dir for both the Windows
// canonical path (backslash) and the POSIX canonical path (forward slash),
// independent of the host's filepath separator.
//
// Negative-control: replacing livenessWorkingDir(canonicalExe) with
// filepath.Dir(canonicalExe) in liveness_task.go makes the first case below
// return "." on a non-Windows `go test` host — this test then fails there
// (and TestInstallLivenessTask_HappyPath's WorkingDirectory fragment too).
// On a Windows host filepath.Dir would still pass, which is exactly why the
// pre-fix bug was invisible to a Windows-only CI run.
func TestLivenessWorkingDir_OSIndependent(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		want string
	}{
		{
			name: "windows-backslash-path",
			exe:  `C:\Users\test\.local\bin\mcphub.exe`,
			want: `C:\Users\test\.local\bin`,
		},
		{
			name: "posix-forward-slash-path",
			exe:  "/home/test/.local/bin/mcphub",
			want: "/home/test/.local/bin",
		},
		{
			name: "no-separator-returns-self",
			exe:  "mcphub.exe",
			want: "mcphub.exe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := livenessWorkingDir(tc.exe); got != tc.want {
				t.Errorf("livenessWorkingDir(%q) = %q, want %q (must be host-OS-independent)", tc.exe, got, tc.want)
			}
		})
	}
}

func decodeUTF16LEBOMForTest(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("missing UTF-16 LE BOM; first bytes=% x", b[:min(len(b), 8)])
	}
	if (len(b)-2)%2 != 0 {
		t.Fatalf("UTF-16 LE payload has odd byte length: %d", len(b)-2)
	}
	units := make([]uint16, 0, (len(b)-2)/2)
	for i := 2; i < len(b); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(units))
}

// TestUninstallLivenessTask_DeletesByName asserts the symmetric teardown
// deletes the LivenessTaskName via the scheduler factory seam.
func TestUninstallLivenessTask_DeletesByName(t *testing.T) {
	a := NewAPI()
	f := newLivenessTaskScheduler()
	installTestScheduler(t, f)

	if err := a.UninstallLivenessTask(); err != nil {
		t.Fatalf("UninstallLivenessTask: %v", err)
	}
	dels := f.calls()
	if len(dels) != 1 || dels[0] != LivenessTaskName {
		t.Errorf("Delete calls = %v, want exactly [%q]", dels, LivenessTaskName)
	}
}
