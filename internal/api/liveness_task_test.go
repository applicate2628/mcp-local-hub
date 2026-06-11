package api

import (
	"errors"
	"strings"
	"testing"
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
	f := &apiSurfacesFakeScheduler{}
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
	body := string(imports[0].xml)
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

// TestInstallLivenessTask_Idempotent asserts running install twice is safe
// (ImportXML overwrites via schtasks /Create /XML /F on Windows).
func TestInstallLivenessTask_Idempotent(t *testing.T) {
	a := NewAPI()
	f := &apiSurfacesFakeScheduler{}
	installTestScheduler(t, f)
	installTestCanonicalMcphubPath(t, `C:\Users\test\.local\bin\mcphub.exe`)
	installTestCurrentWindowsUser(t, "test")

	if err := a.InstallLivenessTask(); err != nil {
		t.Fatalf("first InstallLivenessTask: %v", err)
	}
	if err := a.InstallLivenessTask(); err != nil {
		t.Fatalf("second InstallLivenessTask (idempotent): %v", err)
	}
	if got := len(f.importCalls()); got != 2 {
		t.Errorf("expected 2 ImportXML calls, got %d", got)
	}
}

// TestInstallLivenessTask_PropagatesImportXMLError asserts a scheduler failure
// is surfaced verbatim — the install path does not swallow errors.
func TestInstallLivenessTask_PropagatesImportXMLError(t *testing.T) {
	a := NewAPI()
	want := errors.New("simulated schtasks failure")
	f := &apiSurfacesFakeScheduler{importXMLErr: want}
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

// TestUninstallLivenessTask_DeletesByName asserts the symmetric teardown
// deletes the LivenessTaskName via the scheduler factory seam.
func TestUninstallLivenessTask_DeletesByName(t *testing.T) {
	a := NewAPI()
	f := &apiSurfacesFakeScheduler{}
	installTestScheduler(t, f)

	if err := a.UninstallLivenessTask(); err != nil {
		t.Fatalf("UninstallLivenessTask: %v", err)
	}
	dels := f.calls()
	if len(dels) != 1 || dels[0] != LivenessTaskName {
		t.Errorf("Delete calls = %v, want exactly [%q]", dels, LivenessTaskName)
	}
}
