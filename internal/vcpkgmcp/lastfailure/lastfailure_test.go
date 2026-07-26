package lastfailure

import (
	"os"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/discovery"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
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

// TestScanDiagnostics_ScoutPassTraps_NeverMatch covers traps 1, 3, 4, 5 from
// the 2026-07-26 scout pass over 618 real buildtrees log files: a
// successful patch's non-empty stderr, a comment containing "error", an
// echoed command-line flag, and CMake cache variable NAMES containing
// "ERROR" — none of these are a genuine diagnostic and none must match.
func TestScanDiagnostics_ScoutPassTraps_NeverMatch(t *testing.T) {
	trapLines := []string{
		// Trap 1: a non-empty *-err.log from a SUCCESSFUL patch apply —
		// verified real sample (boost-atomic\patch-wingpl-0-err.log and
		// three siblings): non-empty stderr is NOT evidence of failure.
		`Checking patch fix-cmake.patch...`,
		`Applied patch fix-cmake.patch cleanly.`,
		// Trap 3: comment text containing the word "error" — verified real
		// sample (boost-atomic\config-wingpl-rel-ninja.log).
		`# A missing CMake input file is not an error.`,
		// Trap 4: an echoed command-line flag containing "error" — verified
		// real sample (same file, a printed ctest invocation).
		`--no-tests=error`,
		// Trap 5: CMake cache variable NAMES containing "ERROR" — verified
		// real sample (boost-atomic\config-wingpl-rel-CMakeCache.txt.log).
		`CMAKE_ERROR_ON_ABSOLUTE_INSTALL_DESTINATION:UNINITIALIZED=ON`,
		`CMAKE_ERROR_DEPRECATED:INTERNAL=OFF`,
		// A CMake capability-probe result line containing "Failed" (not
		// "FAILED:") — verified real sample (boost-thread\config-wingpl-out.log).
		`-- Performing Test CMAKE_HAVE_LIBC_PTHREAD - Failed`,
	}
	diags := ScanDiagnostics([]byte(strings.Join(trapLines, "\n")))
	if len(diags) != 0 {
		t.Fatalf("scout-pass trap lines matched as diagnostics: %+v (must be zero)", diags)
	}
}

// TestScanDiagnostics_RealScoutPassSample_MSVCFatalErrorPlusNinjaFailed
// reproduces the exact verbatim sample from the scout pass
// (boost-atomic\config-wingpl-rel-CMakeConfigureLog.yaml.log): a real MSVC
// fatal error immediately followed by ninja's own FAILED: summary line and
// a non-interrupted "subcommand failed" tail (as opposed to "interrupted by
// user").
func TestScanDiagnostics_RealScoutPassSample_MSVCFatalErrorPlusNinjaFailed(t *testing.T) {
	content := strings.Join([]string{
		`src.cxx(1): fatal error C1083: Cannot open include file: 'pthread.h': No such file or directory`,
		`FAILED: [code=2] CMakeFiles/cmTC_e5bae.dir/src.cxx.obj`,
		`ninja: build stopped: subcommand failed.`,
	}, "\n")

	diags := ScanDiagnostics([]byte(content))
	if len(diags) != 2 {
		t.Fatalf("got %d diagnostics, want 2 (fatal error + ninja FAILED): %+v", len(diags), diags)
	}
	if diags[0].Severity != "error" || diags[0].Line != 1 || !strings.Contains(diags[0].Text, "C1083") {
		t.Errorf("first diagnostic = %+v, want the normalized fatal error", diags[0])
	}
	if diags[1].Severity != "error" || diags[1].File != "CMakeFiles/cmTC_e5bae.dir/src.cxx.obj" {
		t.Errorf("second diagnostic = %+v, want the ninja FAILED target", diags[1])
	}
	if DetectInterrupted([]byte(content)) {
		t.Error("\"subcommand failed\" must NOT be classified as a user interrupt")
	}
}

func TestDetectInterrupted(t *testing.T) {
	interrupted := "FAILED: [code=1] foo.obj\nUser interrupt\nninja: build stopped: interrupted by user.\n"
	if !DetectInterrupted([]byte(interrupted)) {
		t.Error("expected the real interrupt sample to be detected")
	}
	notInterrupted := "FAILED: [code=2] foo.obj\nninja: build stopped: subcommand failed.\n"
	if DetectInterrupted([]byte(notInterrupted)) {
		t.Error("a genuine subcommand failure must not be classified as interrupted")
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

// TestLastFailure_BuildInterrupted_NotReportedAsFailure is the integration
// mutation proof for scout-pass trap 2: a "FAILED: [code=1]" line followed
// by "User interrupt" / "ninja: build stopped: interrupted by user." must
// be classified build_interrupted, never reported as a genuine build
// defect (which would misdirect the operator into "fixing" nothing).
func TestLastFailure_BuildInterrupted_NotReportedAsFailure(t *testing.T) {
	args := Args{
		Port:           "interruptedlib",
		BuildtreesRoot: "testdata/failing_port/buildtrees",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonBuildInterrupted {
		t.Fatalf("got status=%v reason=%v, want unknown/build_interrupted; result=%+v", res.Status, res.Reason, res)
	}
}

// TestLastFailure_PatchPhaseFileRecognized_InSituWithRealFailure exercises
// patch-phase file classification alongside a REAL install-phase failure
// (somelib): the patch phase's own successful, non-empty stderr must not
// change which phase/diagnostic gets reported.
func TestLastFailure_PatchPhaseFileRecognized_InSituWithRealFailure(t *testing.T) {
	args := Args{
		Port:           "somelib",
		BuildtreesRoot: "testdata/failing_port/buildtrees",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusFailed {
		t.Fatalf("status = %v, want failed (from the real install-phase diagnostic)", res.Status)
	}
	if res.Phase != PhaseBuild {
		t.Errorf("phase = %q, want build — the patch phase succeeding must not change which phase is reported", res.Phase)
	}
	foundPatchLog := false
	for _, p := range res.LogPaths {
		if strings.Contains(p, "patch-cl-0") {
			foundPatchLog = true
		}
	}
	if !foundPatchLog {
		t.Error("expected the patch-phase log to be classified and listed in log_paths")
	}
}

// TestLastFailure_SuccessfulPatchAlone_NeverFabricatesFailure is the
// isolated integration mutation proof for scout-pass trap 1: patchonlylib's
// ONLY non-empty log anywhere in its buildtrees is patch-cl-0-err.log, and
// its content is a SUCCESSFUL patch-apply transcript ("Checking patch...",
// "Applied patch...cleanly."), not a diagnostic. No other phase log
// carries any content at all, so if a regression treated "stderr
// non-empty" as failure evidence (rather than requiring an anchored
// diagnostic shape), this specific fixture would flip from the correct
// unknown/no_diagnostic_found to a fabricated `failed`.
func TestLastFailure_SuccessfulPatchAlone_NeverFabricatesFailure(t *testing.T) {
	args := Args{
		Port:           "patchonlylib",
		BuildtreesRoot: "testdata/failing_port/buildtrees",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoDiagnosticFound {
		t.Fatalf("got status=%v reason=%v, want unknown/no_diagnostic_found — a successful patch's non-empty stderr must never be treated as failure evidence; result=%+v", res.Status, res.Reason, res)
	}
	if len(res.Diagnostics) != 0 {
		t.Fatalf("fabricated diagnostics from a successful patch's non-empty stderr: %+v", res.Diagnostics)
	}
}

// TestLastFailure_CapabilityProbeLogFallback_NotedAsSuch: no primary phase
// log shows a failure, but a config-<triplet>-<cfg>-CMakeConfigureLog.yaml.log
// artifact (a try_compile capability-probe dump) does. The tool must still
// surface the diagnostic (never silently drop real evidence) but flag it
// with the capability-probe note so a caller does not over-trust it as
// strongly as a primary-phase-log diagnostic.
func TestLastFailure_CapabilityProbeLogFallback_NotedAsSuch(t *testing.T) {
	args := Args{
		Port:           "probelib",
		BuildtreesRoot: "testdata/failing_port/buildtrees",
	}
	res := LastFailure(args, testDeps())
	if res.Status != evidence.StatusFailed {
		t.Fatalf("status = %v, want failed (recovered from the capability-probe log); result=%+v", res.Status, res)
	}
	if !containsNote(res.Notes, NoteDiagnosticFromCapabilityProbeLog) {
		t.Errorf("notes = %v, want diagnostic_from_capability_probe_log", res.Notes)
	}
	if !hasErrorDiagnostic(res.Diagnostics, "C1083") {
		t.Errorf("diagnostics = %+v, want the C1083 fatal error recovered from the probe log", res.Diagnostics)
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
