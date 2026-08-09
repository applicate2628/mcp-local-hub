package lastfailure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Regression tests for the FIELD failure reported against the shipped
// vcpkg-mcp 0.1.0: three separate real libmesh:cl failures all returned
// unknown(no_diagnostic_found) while build-cl-dbg-err.log held an
// unambiguous first error each time.
//
// The failure path was vcpkg -> autotools make -> NMAKE -> a sub-cmake
// --build -> clang-cl / lld-link, and it had TWO independent causes:
//
//  1. build-<triplet>-<cfg>-{out,err}.log was not classified as a phase log
//     at all, so it was listed in log_paths but never diagnostic-scanned.
//     Fixing only the patterns would NOT have fixed the operator's case.
//  2. None of the three real diagnostic shapes was recognized: clang-cl
//     emits the MSVC position shape with NO diagnostic code, and lld-link
//     names itself instead of a source position.

// operatorDiagnosticLines are the operator's ACTUAL first errors, verbatim.
var operatorDiagnosticLines = []string{
	`libsrc/general/mystring.cpp(63,15): error: definition of dllimport static field not allowed`,
	`libsrc/core/bitarray.cpp(164,13): error: cannot use 'throw' with exceptions disabled`,
	`lld-link: error: undefined symbol: __declspec(dllimport) void __cdecl nglib::Ng_Init(void)`,
}

// TestScanDiagnostics_OperatorNestedBuildLines covers cause 2.
func TestScanDiagnostics_OperatorNestedBuildLines(t *testing.T) {
	for _, line := range operatorDiagnosticLines {
		diags := ScanDiagnostics([]byte(line))
		if len(diags) != 1 {
			t.Errorf("got %d diagnostics, want 1 for a real operator failure line:\n  %s", len(diags), line)
			continue
		}
		if diags[0].Severity != SeverityError {
			t.Errorf("severity = %q, want error, for:\n  %s", diags[0].Severity, line)
		}
		if !ContainsFailureDiagnostic(diags) {
			t.Errorf("a genuine compiler/linker error must satisfy the F6 failure gate:\n  %s", line)
		}
	}

	// The position-bearing lines must keep their file and line.
	if d := ScanDiagnostics([]byte(operatorDiagnosticLines[0])); len(d) == 0 {
		t.Error("no diagnostic for the mystring.cpp line")
	} else if d[0].File != "libsrc/general/mystring.cpp" || d[0].Line != 63 {
		t.Errorf("got file=%q line=%d, want libsrc/general/mystring.cpp:63", d[0].File, d[0].Line)
	}
	// The driver-emitted line has no source position; the driver is the locator.
	if d := ScanDiagnostics([]byte(operatorDiagnosticLines[2])); len(d) == 0 {
		t.Error("no diagnostic for the lld-link line")
	} else if d[0].File != "lld-link" {
		t.Errorf("got file=%q, want lld-link", d[0].File)
	}
}

// TestIsWrapperNoise_Predicate pins the wrapper-noise contract directly.
//
// Recorded honestly: as of this change NO diagnostic pattern in this file
// matches an NMAKE U-code line, so the end-to-end NMAKE tests pass with or
// without isWrapperNoise (verified by mutation: neutering the predicate left
// them green). The guard is therefore a FORWARD guard, not a live bug fix —
// it exists so that relaxing a pattern later, exactly as the clang-cl fix
// relaxed msvcCompileDiagRE, cannot silently promote a causeless wrapper line
// to a headline diagnostic. This test is what gives that guard teeth.
func TestIsWrapperNoise_Predicate(t *testing.T) {
	noise := []string{
		`NMAKE : fatal error U1077: 'cd' : return code '0x2'`,
		`NMAKE : fatal error U1073: don't know how to make 'all'`,
		`NMAKE: error U1077: 'x' : return code '0x1'`,
	}
	for _, l := range noise {
		if !isWrapperNoise(l) {
			t.Errorf("isWrapperNoise(%q) = false, want true — a causeless wrapper line "+
				"must never be eligible to become the headline diagnostic", l)
		}
	}

	// It must stay narrow: a real diagnostic, and an NMAKE line that is NOT a
	// U-code wrapper failure, must both pass through.
	notNoise := append([]string{
		`NMAKE : warning U4010: too many rules for target`,
		`nmake.cpp(12): error C2065: 'x': undeclared identifier`,
	}, operatorDiagnosticLines...)
	for _, l := range notNoise {
		if isWrapperNoise(l) {
			t.Errorf("isWrapperNoise(%q) = true, want false — the guard must not swallow "+
				"real diagnostics or non-wrapper lines", l)
		}
	}
}

// TestScanDiagnostics_NMAKEWrapperNoiseNeverMatches: the U1077 cascade at the
// tail carries no cause. Reporting it would hand the operator "return code
// 0x2" instead of the real error, so it must never match — this is the F6
// principle (a line that establishes nothing cannot establish `failed`)
// applied one wrapper layer up.
func TestScanDiagnostics_NMAKEWrapperNoiseNeverMatches(t *testing.T) {
	noise := []string{
		`NMAKE : fatal error U1077: 'cd' : return code '0x2'`,
		`NMAKE : fatal error U1077: '"C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Tools\MSVC\14.39.33519\bin\HostX64\x64\nmake.exe"' : return code '0x2'`,
		`NMAKE : fatal error U1073: don't know how to make 'all'`,
	}
	if diags := ScanDiagnostics([]byte(strings.Join(noise, "\n"))); len(diags) != 0 {
		t.Fatalf("NMAKE wrapper lines matched as diagnostics: %+v (must be zero)", diags)
	}
}

// TestScanDiagnostics_FirstErrorPreferredOverWrapperTail: the real error is
// thousands of lines above a tail of wrapper noise. The FIRST recognized
// diagnostic must be the headline.
func TestScanDiagnostics_FirstErrorPreferredOverWrapperTail(t *testing.T) {
	var b strings.Builder
	b.WriteString("-- Building for: NMake Makefiles\n")
	b.WriteString(operatorDiagnosticLines[0] + "\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("some/other/file.cpp: ordinary build chatter\n")
	}
	// The U1077 cascade, one per wrapper layer.
	for i := 0; i < 4; i++ {
		b.WriteString(`NMAKE : fatal error U1077: 'cd' : return code '0x2'` + "\n")
	}

	diags := ScanDiagnostics([]byte(b.String()))
	if len(diags) == 0 {
		t.Fatal("no diagnostic recovered from a nested build log")
	}
	if !strings.Contains(diags[0].Text, "definition of dllimport static field not allowed") {
		t.Fatalf("headline diagnostic = %q, want the FIRST real error, not the wrapper tail", diags[0].Text)
	}
	for _, d := range diags {
		if strings.Contains(d.Text, "U1077") {
			t.Errorf("wrapper noise leaked into diagnostics: %+v", d)
		}
	}
}

// TestLastFailure_NestedBuildPhaseLog_OperatorFieldCase is the end-to-end
// reproduction of the reported field failure, on the operator's own filename
// shape and content. It covers cause 1 AND cause 2 together.
func TestLastFailure_NestedBuildPhaseLog_OperatorFieldCase(t *testing.T) {
	root := t.TempDir()
	portDir := filepath.Join(root, "libmesh")
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(portDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("cl.vcpkg_abi_info.txt", "abi\n")
	write("config-cl-out.log", "-- Configuring done\n-- Generating done\n")
	// The real error, buried under the wrapper cascade, in the log that was
	// previously never scanned.
	write("build-cl-dbg-err.log", strings.Join([]string{
		operatorDiagnosticLines[0],
		operatorDiagnosticLines[1],
		operatorDiagnosticLines[2],
		`NMAKE : fatal error U1077: 'cd' : return code '0x2'`,
		`NMAKE : fatal error U1077: 'nmake.exe' : return code '0x2'`,
	}, "\n")+"\n")

	res := LastFailure(Args{Port: "libmesh", Triplet: "cl", BuildtreesRoot: root}, testDeps())

	if res.Status != evidence.StatusFailed {
		t.Fatalf("status=%v reason=%v, want failed — this is the shipped-version field failure "+
			"(three real libmesh:cl builds reported unknown/no_diagnostic_found); result=%+v",
			res.Status, res.Reason, res)
	}
	if res.Phase != PhaseBuild {
		t.Errorf("phase = %q, want build", res.Phase)
	}
	if len(res.Diagnostics) == 0 {
		t.Fatal("no diagnostics returned")
	}
	if !strings.Contains(res.Diagnostics[0].Text, "definition of dllimport static field not allowed") {
		t.Errorf("headline diagnostic = %q, want the FIRST real error", res.Diagnostics[0].Text)
	}
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Text, "U1077") {
			t.Errorf("wrapper noise reported as a diagnostic: %+v", d)
		}
	}
	// The build log must be in log_paths as a SCANNED phase log.
	found := false
	for _, p := range res.LogPaths {
		if strings.Contains(p, "build-cl-dbg-err.log") {
			found = true
		}
	}
	if !found {
		t.Errorf("log_paths = %v, want the build-phase log listed", res.LogPaths)
	}
}

// TestLastFailure_NMAKENoiseAlone_NeverEstablishesFailure: composition with
// F6. A build log whose ONLY content is the causeless wrapper cascade cannot
// establish `failed` — the build did fail, but this tool cannot say what
// failed, and saying "return code 0x2" would be a confident non-answer.
func TestLastFailure_NMAKENoiseAlone_NeverEstablishesFailure(t *testing.T) {
	root := t.TempDir()
	portDir := filepath.Join(root, "noiselib")
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, content := range map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-dbg-err.log": `NMAKE : fatal error U1077: 'cd' : return code '0x2'` + "\n" +
			`NMAKE : fatal error U1077: 'nmake.exe' : return code '0x2'` + "\n",
	} {
		if err := os.WriteFile(filepath.Join(portDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	res := LastFailure(Args{Port: "noiselib", Triplet: "cl", BuildtreesRoot: root}, testDeps())
	if res.Status == evidence.StatusFailed {
		t.Fatalf("a causeless NMAKE wrapper cascade established `failed`: %+v", res)
	}
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoDiagnosticFound {
		t.Fatalf("got status=%v reason=%v, want unknown/no_diagnostic_found", res.Status, res.Reason)
	}
	if len(res.LogPaths) == 0 {
		t.Error("log_paths must still be returned so the operator can read further")
	}
}

// TestLastFailure_ClangClErrorInProbeLogStillCapabilityProbeOnly: composition
// with F7. The new patterns make the EXTRACTOR find more; they do not weaken
// the gate that decides what a find MEANS. An error recovered only from a
// try_compile dump stays unknown(capability_probe_only) even now that its
// shape is recognized.
func TestLastFailure_ClangClErrorInProbeLogStillCapabilityProbeOnly(t *testing.T) {
	root := t.TempDir()
	portDir := filepath.Join(root, "probelib2")
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, content := range map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"config-cl-out.log":     "-- Configuring done\n",
		"config-cl-rel-CMakeConfigureLog.yaml.log": "        " +
			`src.cxx(1,1): error: no member named 'pthread_create'` + "\n",
	} {
		if err := os.WriteFile(filepath.Join(portDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	res := LastFailure(Args{Port: "probelib2", Triplet: "cl", BuildtreesRoot: root}, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonCapabilityProbeOnly {
		t.Fatalf("got status=%v reason=%v, want unknown/capability_probe_only — a newly-recognized "+
			"shape found ONLY in a try_compile dump is still not a port failure; result=%+v",
			res.Status, res.Reason, res)
	}
}
