package lastfailure

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Regression tests for the three FIELD defects reported 2026-07-26 by the
// operator driving the shipped binary against their real vcpkg tree
// (libmesh / fparser under a nested vcpkg -> make -> NMAKE -> clang-cl build):
//
//	D-A  the one actionable line was returned LAST, buried under 50 repeated
//	     clang-cl warnings, because diagnostics[] had no ordering at all.
//	D-B  exact_command returned a make TRACE line instead of the reproducible
//	     top-level vcpkg invocation (a regression of the PhaseBuild round).
//	D-C  notes asserted facts about the BUILD that the tool only observed
//	     about its own INPUT.

// operatorClangClWarning is the exact repeated noise line from the field
// report ("unknown argument ignored in clang-cl '-fopenmp'", repeated dozens
// of times). It is a real, correctly-recognized diagnostic — this is a
// RANKING defect, not a filtering one, so it must still be returned.
const operatorClangClWarning = `clang-cl: warning: unknown argument ignored in clang-cl: '-fopenmp' [-Wunknown-argument]`

// operatorLinkError is the single actionable line from the same report.
const operatorLinkError = `fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals`

// writeBuildPhasePort writes a port dir whose build-phase log holds content.
func writeBuildPhasePort(t *testing.T, port string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	portDir := filepath.Join(root, port)
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", portDir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(portDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func warningFlood(n int) string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, operatorClangClWarning)
	}
	return strings.Join(lines, "\n") + "\n"
}

// --- D-A: errors must be reachable without the caller filtering -----------

// TestLastFailure_ErrorRankedFirst_AboveWarningFlood is the direct
// reproduction of the field report: 50 warnings and ONE error, error last in
// file order. The consumer must not have to filter 50 entries to find the one
// actionable line.
func TestLastFailure_ErrorRankedFirst_AboveWarningFlood(t *testing.T) {
	root := writeBuildPhasePort(t, "fparser", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-rel-err.log":  warningFlood(50) + operatorLinkError + "\n",
	})

	res := LastFailure(Args{Port: "fparser", Triplet: "cl", BuildtreesRoot: root}, testDeps())

	if res.Status != evidence.StatusFailed {
		t.Fatalf("status=%v reason=%v, want failed — the LNK1120 error establishes a failure; result=%+v",
			res.Status, res.Reason, res)
	}
	if len(res.Diagnostics) == 0 {
		t.Fatal("no diagnostics returned at all")
	}
	// (1) The error must SURVIVE a warning flood. A per-file cap applied in
	// file order silently drops an error that trails enough warnings.
	if !hasErrorDiagnostic(res.Diagnostics, "LNK1120") {
		t.Fatalf("the ONE actionable error was dropped entirely by the warning flood; "+
			"got %d diagnostics, first=%+v", len(res.Diagnostics), res.Diagnostics[0])
	}
	// (2) It must be REACHABLE without filtering: errors sort ahead of warnings.
	if res.Diagnostics[0].Severity != SeverityError {
		t.Errorf("diagnostics[0] = %+v, want an ERROR first — the actionable line must not be "+
			"buried under warnings the consumer has to filter out itself", res.Diagnostics[0])
	}
	if !strings.Contains(res.Diagnostics[0].Text, "LNK1120") {
		t.Errorf("diagnostics[0].Text = %q, want the LNK1120 error", res.Diagnostics[0].Text)
	}
	// (3) The additive top-level field says it outright.
	if res.FirstError == nil {
		t.Fatal("first_error is nil while an error-severity diagnostic exists")
	}
	if !strings.Contains(res.FirstError.Text, "LNK1120") {
		t.Errorf("first_error = %+v, want the LNK1120 error", *res.FirstError)
	}
	// (4) Warnings are still returned — this is a ranking fix, not a filter.
	warnings := 0
	for _, d := range res.Diagnostics {
		if d.Severity == "warning" {
			warnings++
		}
	}
	if warnings == 0 {
		t.Error("warnings were dropped; D-A is a RANKING defect, warnings must still be returned")
	}
}

// TestLastFailure_DiagnosticOrdering_SeverityThenFirstOccurrence pins the
// SEVERITY and FIRST-OCCURRENCE keys of the documented ordering rule: severity
// first, then (all four lines here being tier=specific source-position
// diagnostics) first-occurrence order preserved — a stable sort, not an
// arbitrary one. The tier key that sits between the two is pinned separately
// in diagnostic_tier_test.go.
func TestLastFailure_DiagnosticOrdering_SeverityThenFirstOccurrence(t *testing.T) {
	root := writeBuildPhasePort(t, "orderlib", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-rel-err.log": strings.Join([]string{
			`w1.cpp(1): warning C4244: first warning`,
			`e1.cpp(2): error C2065: first error`,
			`w2.cpp(3): warning C4244: second warning`,
			`e2.cpp(4): error C2065: second error`,
		}, "\n") + "\n",
	})

	res := LastFailure(Args{Port: "orderlib", Triplet: "cl", BuildtreesRoot: root}, testDeps())
	if len(res.Diagnostics) != 4 {
		t.Fatalf("got %d diagnostics, want all 4 retained: %+v", len(res.Diagnostics), res.Diagnostics)
	}
	wantFiles := []string{"e1.cpp", "e2.cpp", "w1.cpp", "w2.cpp"}
	for i, want := range wantFiles {
		if res.Diagnostics[i].File != want {
			t.Errorf("diagnostics[%d].File = %q, want %q — ordering must be severity, then "+
				"tier, then first-occurrence", i, res.Diagnostics[i].File, want)
		}
	}
}

// TestScanDiagnostics_LateErrorSurvivesWarningFlood is the unit-level guard
// for the same hazard at the scan layer: the per-log cap must never be spent
// entirely on warnings, or a trailing error becomes invisible before any
// ranking can reach it.
func TestScanDiagnostics_LateErrorSurvivesWarningFlood(t *testing.T) {
	content := warningFlood(maxDiagnosticsPerLog+20) + operatorLinkError + "\n"
	diags := ScanDiagnostics([]byte(content))
	if !ContainsFailureDiagnostic(diags) {
		t.Fatalf("an error trailing %d warnings was dropped by the per-log cap; got %d diagnostics",
			maxDiagnosticsPerLog+20, len(diags))
	}
}

// --- bounded reads: a log is never fully materialized before it is capped --

// endlessFS serves an UNBOUNDED stream for any path matching sub. An
// unbounded read of it never terminates, so a test that completes at all is
// itself the proof that the read is bounded before materialization.
type endlessFS struct {
	inner FS
	sub   string
	fill  byte
}

type endlessReader struct{ fill byte }

func (e endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.fill
	}
	return len(p), nil
}
func (endlessReader) Close() error { return nil }

func (f endlessFS) Stat(p string) (os.FileInfo, error) {
	if strings.Contains(filepath.ToSlash(p), f.sub) {
		return regularTestFileInfo{name: p}, nil
	}
	return f.inner.Stat(p)
}
func (f endlessFS) OpenDir(p string) (DirReader, error) { return f.inner.OpenDir(p) }
func (f endlessFS) Open(p string) (io.ReadCloser, error) {
	if strings.Contains(filepath.ToSlash(p), f.sub) {
		return endlessReader{fill: f.fill}, nil
	}
	return f.inner.Open(p)
}

// TestReadMetadataLimited_BoundsBeforeMaterializing pins the remaining
// metadata-materialization bound at a test-sized limit against a stream with
// no end. Phase logs have their separate streaming guards in logstream_test.go.
func TestReadMetadataLimited_BoundsBeforeMaterializing(t *testing.T) {
	fsys := endlessFS{inner: DefaultFS(), sub: "endless.log", fill: 'x'}
	data, truncated, err := readMetadataLimited(fsys, "endless.log", 64)
	if err != nil {
		t.Fatalf("readMetadataLimited: %v", err)
	}
	if !truncated {
		t.Error("truncated = false for a stream longer than the limit")
	}
	if len(data) != 64 {
		t.Errorf("read %d bytes, want exactly the 64-byte limit", len(data))
	}
}

// TestLastFailure_OversizeLog_FailsClosedNotSilentlyTruncated: a log larger
// than the tool will materialize must NOT yield a confident verdict from the
// prefix it managed to read — the unread tail can hold a later error or an
// interrupt marker that changes the answer entirely.
func TestLastFailure_OversizeLog_FailsClosedNotSilentlyTruncated(t *testing.T) {
	root := writeBuildPhasePort(t, "hugelib", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-rel-err.log":  `boom.cpp(1): error C2065: 'x': undeclared identifier` + "\n",
	})
	deps := testDeps()
	// '\n' fill keeps the line scanner happy, so the ONLY thing under test is
	// the size bound rather than an incidental scanner failure.
	deps.FS = endlessFS{inner: DefaultFS(), sub: "build-cl-rel-err.log", fill: '\n'}

	res := LastFailure(Args{Port: "hugelib", Triplet: "cl", BuildtreesRoot: root}, deps)
	if res.Status == evidence.StatusFailed {
		t.Fatalf("a confident `failed` was issued from a log that was only partly read: %+v", res)
	}
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPhaseLogSizeLimitExceeded {
		t.Fatalf("got status=%v reason=%v, want unknown/phase_log_size_limit_exceeded",
			res.Status, res.Reason)
	}
	if len(res.LogPaths) == 0 {
		t.Error("log_paths must still be returned so the operator can read the log themselves")
	}
}

// --- D-B: exact_command is the top-level vcpkg invocation ------------------

// makeTraceBuildLog is the operator's actual build-phase log shape: it OPENS
// with make's own trace output. Returning its first line as "the exact
// command" hands the operator something that is not a command at all.
const makeTraceBuildLog = `Makefile:36039: update target 'all-recursive' due to: target is .PHONY
make[2]: Entering directory '/r/b/cl/libmesh/src'
` + operatorLinkError + `
NMAKE : fatal error U1077: 'cd' : return code '0x2'
`

// TestLastFailure_ExactCommand_IsVcpkgInvocationNotMakeTrace is the D-B
// regression: a fixture carrying BOTH a real vcpkg invocation (in the
// wrapper) and make trace lines (in the build log). The vcpkg one must win.
func TestLastFailure_ExactCommand_IsVcpkgInvocationNotMakeTrace(t *testing.T) {
	root := writeBuildPhasePort(t, "libmesh", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-dbg-out.log":  makeTraceBuildLog,
	})

	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "build_failed.log")
	const vcpkgInvocation = `C:\vcpkg\vcpkg.exe install libmesh --overlay-ports=C:\vcpkg\overlays\ports --triplet=cl --x-buildtrees-root=r:/b/cl`
	content := "[2026-07-26 09:00:00] triplet=cl\n" +
		"command: " + vcpkgInvocation + "\n" +
		"exit_code: 1\nbuild_failed_count: 1\nfailed_ports:\n- libmesh:cl\n"
	if err := os.WriteFile(wrapper, []byte(content), 0o644); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	res := LastFailure(Args{
		Port:           "libmesh",
		Triplet:        "cl",
		BuildtreesRoot: root,
		BuildFailedLog: wrapper,
	}, testDeps())

	if strings.Contains(res.ExactCommand, "Makefile:") || strings.Contains(res.ExactCommand, ".PHONY") {
		t.Fatalf("exact_command = %q — that is a make TRACE line, not a command. The operator "+
			"pastes this into a shell; a wrong command is worse than no command.", res.ExactCommand)
	}
	if res.ExactCommand != vcpkgInvocation {
		t.Errorf("exact_command = %q, want the recorded top-level vcpkg invocation %q",
			res.ExactCommand, vcpkgInvocation)
	}
}

// TestLastFailure_ExactCommand_AbsentRatherThanWrongLayer: with no wrapper
// there is no recorded top-level invocation anywhere in a vcpkg buildtree.
// The honest answer is the absent form plus a note naming the gap — never a
// plausible-looking line lifted from a nested build tool's output.
func TestLastFailure_ExactCommand_AbsentRatherThanWrongLayer(t *testing.T) {
	root := writeBuildPhasePort(t, "libmesh", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-dbg-out.log":  makeTraceBuildLog,
	})

	res := LastFailure(Args{Port: "libmesh", Triplet: "cl", BuildtreesRoot: root}, testDeps())

	if res.ExactCommand != "" {
		t.Fatalf("exact_command = %q, want the absent form — no top-level vcpkg invocation is "+
			"recoverable without a wrapper file, and guessing one is worse than saying so",
			res.ExactCommand)
	}
	if !containsNote(res.Notes, NoteExactCommandNotRecovered) {
		t.Errorf("notes = %v, want exact_command_not_recovered so the gap is explicit", res.Notes)
	}
}

// TestLastFailure_BuildCommand_CarriesTheSubInvocation: the CMake-recorded
// build sub-invocation is genuinely useful, so it is preserved — in its OWN
// field, correctly labelled as the build layer rather than smuggled into
// exact_command as if it were the vcpkg invocation.
func TestLastFailure_BuildCommand_CarriesTheSubInvocation(t *testing.T) {
	root := writeBuildPhasePort(t, "ninjalib", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"install-cl-rel-out.log": "Run Build Command(s): C:/ninja/ninja.exe -v all\n" +
			`foo.cpp(9): error C2065: 'boom': undeclared identifier` + "\n",
	})

	res := LastFailure(Args{Port: "ninjalib", Triplet: "cl", BuildtreesRoot: root}, testDeps())
	if res.BuildCommand != "C:/ninja/ninja.exe -v all" {
		t.Errorf("build_command = %q, want the recorded build sub-invocation", res.BuildCommand)
	}
	if res.ExactCommand != "" {
		t.Errorf("exact_command = %q, want empty — a build sub-invocation is NOT the top-level "+
			"vcpkg invocation and must not be reported as one", res.ExactCommand)
	}
}

// TestLastFailure_BuildCommand_MatchesTheDiagnosticBearingConfiguration: a
// phase can hold several build configurations. Diagnostics accumulate across
// all of them, so a command taken from "whichever log the loop touched last"
// need not belong to the step that actually failed — the operator would then
// be handed the RELEASE build's command for a DEBUG build's error.
func TestLastFailure_BuildCommand_MatchesTheDiagnosticBearingConfiguration(t *testing.T) {
	root := writeBuildPhasePort(t, "multicfg", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		// dbg is the failing configuration.
		"install-cl-dbg-out.log": "Run Build Command(s): ninja-for-dbg\n",
		"install-cl-dbg-err.log": `boom.cpp(1): error C2065: 'x': undeclared identifier` + "\n",
		// rel succeeded; directory order puts it AFTER dbg, so a
		// last-one-wins association silently picks this one.
		"install-cl-rel-out.log": "Run Build Command(s): ninja-for-rel\n",
	})

	res := LastFailure(Args{Port: "multicfg", Triplet: "cl", BuildtreesRoot: root}, testDeps())
	if res.Status != evidence.StatusFailed {
		t.Fatalf("status=%v reason=%v, want failed; result=%+v", res.Status, res.Reason, res)
	}
	if !strings.Contains(res.DiagnosticLog, "install-cl-dbg-err.log") {
		t.Errorf("diagnostic_log = %q, want the dbg err log that produced the error", res.DiagnosticLog)
	}
	if res.BuildCommand != "ninja-for-dbg" {
		t.Errorf("build_command = %q, want ninja-for-dbg — the command must belong to the same "+
			"build step as the reported diagnostic, not to whichever log was read last",
			res.BuildCommand)
	}
}

// --- D-C: every value describes the observation, never the world -----------

// TestLastFailure_OverlayChainNote_DescribesInputNotTheBuild is the reported
// D-C instance: the operator supplied no overlays while the build actually
// used four. The tool observed only its own INPUT and must say exactly that.
func TestLastFailure_OverlayChainNote_DescribesInputNotTheBuild(t *testing.T) {
	res := LastFailure(Args{
		Port:           "somelib",
		BuildtreesRoot: absPath(t, "testdata/failing_port/buildtrees"),
	}, testDeps())

	if !containsNote(res.Notes, NoteOverlayChainNotSupplied) {
		t.Fatalf("notes = %v, want overlay_chain_not_supplied", res.Notes)
	}
	for _, n := range res.Notes {
		if strings.Contains(string(n), "none") || strings.Contains(string(n), "builtin_ports_only") {
			t.Errorf("note %q asserts a fact about the BUILD (that no overlays were in play) "+
				"from an observation about the tool's own INPUT (none were supplied). The build "+
				"in the field report used four overlay directories.", n)
		}
	}
}

// TestLastFailure_WrapperNotSuppliedVsNotFoundVsUnreadable pins the three
// distinct observations apart. Each names a different operator remedy, and
// none may be phrased as a fact about the wrapper file's content.
func TestLastFailure_WrapperNotSuppliedVsNotFoundVsUnreadable(t *testing.T) {
	root := absPath(t, "testdata/failing_port/buildtrees")

	// (a) not supplied: the tool never looks for a wrapper (it is never
	// auto-discovered), so it cannot know whether one exists.
	res := LastFailure(Args{Port: "somelib", BuildtreesRoot: root}, testDeps())
	if !containsNote(res.Notes, NoteWrapperNotSupplied) {
		t.Errorf("notes = %v, want wrapper_not_supplied", res.Notes)
	}
	for _, n := range res.Notes {
		if string(n) == "wrapper_absent" {
			t.Error("wrapper_absent claims the FILE is absent; the tool only observed that the " +
				"caller supplied no path and never probed for one")
		}
	}

	// (b) supplied but not on disk: a verified absence of that path.
	missing := filepath.Join(t.TempDir(), "no-such-build_failed.log")
	res = LastFailure(Args{Port: "somelib", BuildtreesRoot: root, BuildFailedLog: missing}, testDeps())
	if !containsNote(res.Notes, NoteWrapperNotFound) {
		t.Errorf("notes = %v, want wrapper_not_found for a supplied path that does not exist", res.Notes)
	}
	if containsNote(res.Notes, NoteWrapperMalformed) {
		t.Error("a missing file was reported as MALFORMED — that is a claim about content the " +
			"tool never read")
	}

	// (c) supplied and present but unreadable: not absence, not malformation.
	dir := t.TempDir()
	denied := filepath.Join(dir, "build_failed.log")
	if err := os.WriteFile(denied, []byte("command: vcpkg.exe install foo\n"), 0o644); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	deps := testDeps()
	deps.FS = failingFS{inner: DefaultFS(), failSub: "build_failed.log", failRead: true}
	res = LastFailure(Args{Port: "somelib", BuildtreesRoot: root, BuildFailedLog: denied}, deps)
	if !containsNote(res.Notes, NoteWrapperUnreadable) {
		t.Errorf("notes = %v, want wrapper_unreadable for an access-denied read", res.Notes)
	}
	if containsNote(res.Notes, NoteWrapperMalformed) || containsNote(res.Notes, NoteWrapperNotFound) {
		t.Error("an unreadable wrapper was reported as malformed or absent — both are claims " +
			"about a file the tool failed to read at all")
	}
}

// TestLastFailure_OverlayChainFromConfigFile_NotLabelledExplicitParam: the
// chain came from a vcpkg-configuration.json on disk, not from the caller's
// overlays param. Reporting the wrong provenance makes the answer
// unverifiable — the caller checks the source the note names.
func TestLastFailure_OverlayChainFromConfigFile_NotLabelledExplicitParam(t *testing.T) {
	vcpkgRoot := t.TempDir()
	cfg := filepath.Join(vcpkgRoot, "vcpkg-configuration.json")
	if err := os.WriteFile(cfg, []byte(`{"overlay-ports":["C:\\overlays\\ports"]}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	res := LastFailure(Args{
		Port:           "somelib",
		Root:           vcpkgRoot,
		BuildtreesRoot: absPath(t, "testdata/failing_port/buildtrees"),
	}, testDeps())

	if len(res.OverlayChain) != 1 {
		t.Fatalf("overlay_chain = %v, want the config-file chain", res.OverlayChain)
	}
	if !containsNote(res.Notes, NoteOverlayChainFromVcpkgConfiguration) {
		t.Errorf("notes = %v, want overlay_chain_from_vcpkg_configuration", res.Notes)
	}
	if containsNote(res.Notes, NoteOverlayChainFromParam) {
		t.Error("a chain read from vcpkg-configuration.json was labelled as coming from an " +
			"explicit param the caller never passed")
	}
}

// TestLastFailure_ExplicitRootThatIsNotAVcpkgTree_NotReportedAsUnspecified:
// the caller DID specify a root. Saying "not specified" is a false statement
// about the tool's own input.
func TestLastFailure_ExplicitRootThatIsNotAVcpkgTree_NotReportedAsUnspecified(t *testing.T) {
	res := LastFailure(Args{Port: "somelib", Root: t.TempDir()}, testDeps())
	if res.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown; result=%+v", res.Status, res)
	}
	if string(res.Reason) == "root_not_specified" {
		t.Fatal("an explicitly supplied root was reported as root_not_specified — the root WAS " +
			"specified; what the tool observed is that it could not resolve one from it")
	}
	if res.Reason != ReasonVcpkgRootNotResolved {
		t.Errorf("reason = %v, want %v", res.Reason, ReasonVcpkgRootNotResolved)
	}
}

// TestLastFailure_AbsentBuildtreesRoot_DescribesAbsenceNotItsCause: the probe
// verified the directory is absent. It did NOT verify WHY — attributing it to
// --clean-buildtrees-after-build is a causal claim from a presence probe, and
// it misdirects the operator whose real mistake was a wrong root.
func TestLastFailure_AbsentBuildtreesRoot_DescribesAbsenceNotItsCause(t *testing.T) {
	res := LastFailure(Args{
		Port:           "somelib",
		BuildtreesRoot: absPath(t, "testdata/this_buildtrees_root_does_not_exist"),
	}, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonBuildtreesRootAbsent {
		t.Fatalf("got status=%v reason=%v, want unknown/buildtrees_root_absent", res.Status, res.Reason)
	}
	if string(res.Reason) == "buildtrees_cleaned" {
		t.Error("buildtrees_cleaned attributes a CAUSE the presence probe cannot establish")
	}
}
