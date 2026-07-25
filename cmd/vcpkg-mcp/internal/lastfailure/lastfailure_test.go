package lastfailure

import (
	"os"
	"strings"
	"testing"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/discovery"
	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
)

func testDeps() Deps {
	return Deps{
		FS:     DefaultFS(),
		Getenv: func(string) string { return "" },
		Discovery: discovery.Deps{
			Getenv:      func(string) string { return "" },
			LookPath:    func(string) (string, error) { return "", os.ErrNotExist },
			Getwd:       func() (string, error) { return ".", nil },
			Stat:        os.Stat,
			GOOS:        "windows",
			UserHomeDir: func() (string, error) { return "", os.ErrNotExist },
		},
	}
}

// --- wrapper.go: build_failed.log parsing (optional enrichment) -----------

func TestParseWrapperContent_RealShapedFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/wrapper_ok.log")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	info, ok := ParseWrapperContent(data)
	if !ok {
		t.Fatal("expected ok=true for a well-formed wrapper file")
	}
	if info.Triplet != "cl" {
		t.Errorf("Triplet = %q, want cl", info.Triplet)
	}
	if len(info.OverlayPorts) != 3 {
		t.Errorf("OverlayPorts = %v, want 3 entries in precedence order", info.OverlayPorts)
	}
	if info.BuildtreesRoot != "r:/b/cl" {
		t.Errorf("BuildtreesRoot = %q, want r:/b/cl", info.BuildtreesRoot)
	}
	if info.InstallRoot != "q:/vcpkg-libs/cl" {
		t.Errorf("InstallRoot = %q", info.InstallRoot)
	}
	if info.ExitCode == nil || *info.ExitCode != 1 {
		t.Errorf("ExitCode = %v, want 1", info.ExitCode)
	}
	if len(info.FailedPorts) != 1 || info.FailedPorts[0] != "somelib:cl" {
		t.Errorf("FailedPorts = %v, want [somelib:cl]", info.FailedPorts)
	}
}

func TestParseWrapperContent_Malformed(t *testing.T) {
	data, err := os.ReadFile("testdata/wrapper_malformed.log")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	_, ok := ParseWrapperContent(data)
	if ok {
		t.Fatal("expected ok=false for an unrecognizable file — must degrade, never fabricate")
	}
}

// --- diagnostics.go: shape-anchored matching, not substring scanning ------

func TestScanDiagnostics_FilenameTrapsNeverMatch(t *testing.T) {
	// Verbatim lines observed this session in a real vcpkg buildtrees log
	// (r:\b\wingpl\boost-system\install-wingpl-rel-out.log and
	// r:\b\wingpl\boost-compat\install-wingpl-rel-out.log): a CMake
	// "-- Installing:" status line whose PATH happens to contain the
	// substring "error" as part of a filename. None of these have a
	// recognized diagnostic POSITION shape, so none must match — this is
	// the exact trap the design doc names ("grep error matches filenames
	// like error_estimator.h").
	trapLines := []string{
		`-- Installing: R:/vcpkg-cache/packages/boost-system_wingpl/include/boost/system/detail/error_category.hpp`,
		`-- Installing: R:/vcpkg-cache/packages/boost-system_wingpl/include/boost/system/detail/error_category_impl.hpp`,
		`-- Installing: R:/vcpkg-cache/packages/boost-system_wingpl/include/boost/system/detail/error_code.hpp`,
		`-- Installing: R:/vcpkg-cache/packages/boost-compat_wingpl/include/boost/compat/detail/throw_system_error.hpp`,
		`-- Installing: R:/vcpkg-cache/packages/boost-system_wingpl/include/boost/system/error_category.hpp`,
	}
	diags := ScanDiagnostics([]byte(strings.Join(trapLines, "\n")))
	if len(diags) != 0 {
		t.Fatalf("filename-trap lines matched as diagnostics: %+v (must be zero — this IS the substring-vs-shape mutation proof)", diags)
	}
}

func TestScanDiagnostics_MSVCCompileErrorMatches(t *testing.T) {
	line := `R:\b\cl\somelib\src\somelib-1.0.clean\foo.cpp(42): error C2065: 'undeclared_thing': undeclared identifier`
	diags := ScanDiagnostics([]byte(line))
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %+v", len(diags), diags)
	}
	if diags[0].Line != 42 || diags[0].Severity != "error" {
		t.Errorf("got %+v", diags[0])
	}
}

func TestScanDiagnostics_MSVCWarningWithErrorLikeFilenameStillMatchesAsWarning(t *testing.T) {
	// The inverse edge case: a file whose NAME contains "error" (so a
	// naive substring scan would be right for the wrong reason) genuinely
	// IS the subject of a real, correctly-shaped diagnostic. The matcher
	// must accept this — it discriminates on POSITION/shape, not on
	// whether "error" appears in the path text.
	line := `R:\b\cl\somelib\src\somelib-1.0.clean\error_estimator.h(12): warning C4244: 'initializing': conversion from 'double' to 'int', possible loss of data`
	diags := ScanDiagnostics([]byte(line))
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %+v", len(diags), diags)
	}
	if diags[0].Severity != "warning" || diags[0].Line != 12 {
		t.Errorf("got %+v", diags[0])
	}
}

func TestScanDiagnostics_GCCClangShape(t *testing.T) {
	line := `/src/foo.cpp:10:5: error: 'x' was not declared in this scope`
	diags := ScanDiagnostics([]byte(line))
	if len(diags) != 1 || diags[0].Line != 10 || diags[0].Severity != "error" {
		t.Fatalf("got %+v", diags)
	}
}

// --- lastfailure.go: full orchestration, buildtrees-primary -------------

func TestLastFailure_Case_A_BuildtreesPlusWrapper(t *testing.T) {
	// (a) buildtrees present + wrapper present.
	args := Args{
		BuildtreesRoot: "testdata/failing_port/buildtrees",
		BuildFailedLog: "testdata/wrapper_ok.log",
		// Port omitted on purpose: exactly one failed port in the wrapper.
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusFailed {
		t.Fatalf("status = %v, want failed; result=%+v", res.Status, res)
	}
	if res.FailedTarget != "somelib" {
		t.Errorf("failed_target = %q, want somelib (auto-selected from wrapper)", res.FailedTarget)
	}
	if res.Phase != PhaseBuild {
		t.Errorf("phase = %q, want build (escalated from install)", res.Phase)
	}
	if !containsContextSource(res.ContextSource, SourceBuildtrees) || !containsContextSource(res.ContextSource, SourceWrapperSummary) {
		t.Errorf("context_source = %v, want both buildtrees and wrapper_summary", res.ContextSource)
	}
	if len(res.OverlayChain) != 3 {
		t.Errorf("overlay_chain = %v, want 3 entries recovered from the wrapper (highest-fidelity source)", res.OverlayChain)
	}
	if !hasErrorDiagnostic(res.Diagnostics, "C2065") {
		t.Errorf("diagnostics = %+v, want a C2065 error present", res.Diagnostics)
	}
	if len(res.LogPaths) == 0 {
		t.Error("log_paths must always be populated")
	}
	if res.ExitCode == nil || *res.ExitCode != 1 {
		t.Errorf("exit_code = %v, want 1 (from wrapper)", res.ExitCode)
	}
}

// TestLastFailure_Case_B_BuildtreesOnly_NoWrapper is the mutation proof
// that this tool is NOT wrapper-dependent: same buildtrees fixture as case
// A, with the wrapper file simply not supplied. Must still produce a full
// `failed` answer purely from vcpkg-native artifacts.
func TestLastFailure_Case_B_BuildtreesOnly_NoWrapper(t *testing.T) {
	args := Args{
		Port:           "somelib",
		BuildtreesRoot: "testdata/failing_port/buildtrees",
		// BuildFailedLog deliberately omitted.
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusFailed {
		t.Fatalf("status = %v, want failed WITHOUT a wrapper file; result=%+v", res.Status, res)
	}
	if containsContextSource(res.ContextSource, SourceWrapperSummary) {
		t.Errorf("context_source = %v, must not claim wrapper_summary when no wrapper was supplied", res.ContextSource)
	}
	if !hasErrorDiagnostic(res.Diagnostics, "C2065") {
		t.Errorf("diagnostics = %+v, want a C2065 error present from buildtrees alone", res.Diagnostics)
	}
	if !containsNote(res.Notes, NoteWrapperAbsent) {
		t.Errorf("notes = %v, want wrapper_absent noted", res.Notes)
	}
}

// (c) wrapper present but malformed: must degrade, not error.
func TestLastFailure_Case_C_WrapperMalformed_Degrades(t *testing.T) {
	args := Args{
		Port:           "somelib",
		BuildtreesRoot: "testdata/failing_port/buildtrees",
		BuildFailedLog: "testdata/wrapper_malformed.log",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusFailed {
		t.Fatalf("status = %v, want failed (native path unaffected by a malformed wrapper); result=%+v", res.Status, res)
	}
	if !containsNote(res.Notes, NoteWrapperMalformed) {
		t.Errorf("notes = %v, want wrapper_malformed_ignored", res.Notes)
	}
	if containsContextSource(res.ContextSource, SourceWrapperSummary) {
		t.Errorf("context_source = %v, must not credit a malformed wrapper as a source", res.ContextSource)
	}
}

// (d) buildtrees cleaned: --clean-buildtrees-after-build removed the whole
// triplet's tree. Mutation proof: BuildtreesRoot deliberately points at a
// path that does not exist (the exact real-world shape observed:
// r:\b\cl did not exist at all while r:\b\wingpl survived).
func TestLastFailure_Case_D_BuildtreesCleaned(t *testing.T) {
	args := Args{
		Port:           "somelib",
		BuildtreesRoot: "testdata/this_buildtrees_root_does_not_exist",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonBuildtreesCleaned {
		t.Fatalf("got status=%v reason=%v, want unknown/buildtrees_cleaned", res.Status, res.Reason)
	}
}

func TestLastFailure_PortDirNotFound(t *testing.T) {
	args := Args{
		Port:           "does-not-exist-anywhere",
		BuildtreesRoot: "testdata/failing_port/buildtrees",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPortDirNotFound {
		t.Fatalf("got status=%v reason=%v, want unknown/port_dir_not_found", res.Status, res.Reason)
	}
}

func TestLastFailure_NoPhaseLogsFound(t *testing.T) {
	// Real degenerate case observed this session: r:\b\wingpl\sqlite3\
	// contains only wingpl.vcpkg_abi_info.txt, no phase logs at all —
	// the build never reached the extract phase (or those logs were also
	// cleaned). Must be distinguished from "buildtrees_cleaned" (the
	// directory DOES exist) and must never fabricate a diagnostic.
	args := Args{
		Port:           "sqlite3",
		Triplet:        "wingpl",
		BuildtreesRoot: "testdata/wingpl_like/buildtrees",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoPhaseLogsFound {
		t.Fatalf("got status=%v reason=%v, want unknown/no_phase_logs_found", res.Status, res.Reason)
	}
}

// TestLastFailure_SuccessfulPort_NoDiagnostic_NeverFabricates is the
// integration-level mutation proof for the filename-trap discrimination:
// boost-algorithm's real install log contains several "-- Installing:
// .../error_category.hpp"-shaped lines. If diagnostic matching regressed
// to a naive substring scan, this test would wrongly report status=failed
// with a bogus diagnostic pointing at one of those header files.
func TestLastFailure_SuccessfulPort_NoDiagnostic_NeverFabricates(t *testing.T) {
	args := Args{
		Port:           "boost-algorithm",
		Triplet:        "wingpl",
		BuildtreesRoot: "testdata/wingpl_like/buildtrees",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoDiagnosticFound {
		t.Fatalf("got status=%v reason=%v, want unknown/no_diagnostic_found (logs read, nothing genuinely matched)", res.Status, res.Reason)
	}
	for _, d := range res.Diagnostics {
		if strings.Contains(d.File, "error_category") || strings.Contains(d.File, "error_estimator") {
			t.Fatalf("fabricated a diagnostic from a filename trap: %+v", d)
		}
	}
	if len(res.LogPaths) == 0 {
		t.Error("log_paths must still be populated even when no diagnostic was found")
	}
}

func TestLastFailure_WrapperConfirmsPortDidNotFail(t *testing.T) {
	args := Args{
		Port:           "somelib",
		BuildFailedLog: "testdata/wrapper_confirms_no_failure.log", // lists only otherlib:cl as failed
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok (wrapper positively confirms this port did not fail)", res.Status)
	}
	if !containsNote(res.Notes, NoteWrapperConfirmsNoFailure) {
		t.Errorf("notes = %v, want wrapper_confirms_port_not_failed", res.Notes)
	}
}

func TestLastFailure_MultipleFailedPortsAmbiguous_NeverSilentlyPicks(t *testing.T) {
	args := Args{
		BuildFailedLog: "testdata/wrapper_ok_multi.log", // 3 failed ports, no Port param
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonMultipleFailedPortsAmbiguous {
		t.Fatalf("got status=%v reason=%v, want unknown/multiple_failed_ports_ambiguous", res.Status, res.Reason)
	}
	for _, want := range []string{"somelib", "otherlib", "thirdlib"} {
		if !strings.Contains(res.FailedTarget, want) {
			t.Errorf("failed_target = %q, want it to list %q", res.FailedTarget, want)
		}
	}
}

func TestLastFailure_PortNotSpecified_NoWrapperNoPort(t *testing.T) {
	res := LastFailure(Args{BuildtreesRoot: "testdata/failing_port/buildtrees"}, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPortNotSpecified {
		t.Fatalf("got status=%v reason=%v, want unknown/port_not_specified", res.Status, res.Reason)
	}
}

// --- test helpers ----------------------------------------------------------

func containsContextSource(list []ContextSource, want ContextSource) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsNote(list []Note, want Note) bool {
	for _, n := range list {
		if n == want {
			return true
		}
	}
	return false
}

func hasErrorDiagnostic(diags []Diagnostic, codeSubstr string) bool {
	for _, d := range diags {
		if d.Severity == "error" && strings.Contains(d.Text, codeSubstr) {
			return true
		}
	}
	return false
}
