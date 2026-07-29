package lastfailure

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// This file holds the regression tests for the evidence-integrity findings:
// every case here is one where the tool previously produced a CONFIDENT
// verdict from evidence that does not settle the question.

// failingFS delegates to the real filesystem except for paths matching a
// substring, which fail with a NON-ErrNotExist error — the shape a
// permission denial, sharing violation or transient I/O error takes. Such a
// failure must never be reported as absence.
type failingFS struct {
	inner    FS
	failSub  string
	failStat bool
	failRead bool
}

var errDenied = fs.ErrPermission

func (f failingFS) hit(p string) bool {
	return f.failSub != "" && strings.Contains(filepath.ToSlash(p), f.failSub)
}

func (f failingFS) Stat(p string) (os.FileInfo, error) {
	if f.failStat && f.hit(p) {
		return nil, errDenied
	}
	return f.inner.Stat(p)
}

func (f failingFS) OpenDir(p string) (DirReader, error) { return f.inner.OpenDir(p) }

// Open covers logs and metadata; both are bounded streams.
func (f failingFS) Open(p string) (io.ReadCloser, error) {
	if f.failRead && f.hit(p) {
		return nil, errDenied
	}
	return f.inner.Open(p)
}

// writePortLogs builds a synthetic buildtrees port directory.
func writePortLogs(t *testing.T, port string, files map[string]string) string {
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

// --- F3: root-like parameters must be absolute ---------------------------

// TestLastFailure_RelativeBuildtreesRoot_Refused: a relative root resolves
// against the HUB DAEMON's working directory — not the caller's, and not the
// one the recorded vcpkg invocation used — so any answer derived from it
// describes an unrelated tree while looking fully confident.
func TestLastFailure_RelativeBuildtreesRoot_Refused(t *testing.T) {
	res := LastFailure(Args{
		Port:           "somelib",
		BuildtreesRoot: "testdata/failing_port/buildtrees", // deliberately relative
	}, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonRelativeRoot {
		t.Fatalf("got status=%v reason=%v, want unknown/relative_root — a CWD-bound root must never "+
			"produce a confident diagnosis; result=%+v", res.Status, res.Reason, res)
	}
}

// TestLastFailure_RelativeRootParam_Refused: same rule for `root`.
func TestLastFailure_RelativeRootParam_Refused(t *testing.T) {
	res := LastFailure(Args{Port: "somelib", Root: "some/relative/vcpkg"}, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonRelativeRoot {
		t.Fatalf("got status=%v reason=%v, want unknown/relative_root", res.Status, res.Reason)
	}
}

// TestLastFailure_RelativeWrapperBuildtreesRoot_Refused: vcpkg resolves a
// relative --x-buildtrees-root against the shell that ran it, whose working
// directory this tool has no way to know.
func TestLastFailure_RelativeWrapperBuildtreesRoot_Refused(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "build_failed.log")
	content := "[2026-07-24 21:35:11] triplet=cl\n" +
		"command: vcpkg.exe install foo --x-buildtrees-root=buildtrees --triplet=cl\n" +
		"exit_code: 1\nbuild_failed_count: 1\nfailed_ports:\n- foo:cl\n"
	if err := os.WriteFile(wrapper, []byte(content), 0o644); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	res := LastFailure(Args{BuildFailedLog: wrapper}, testDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonRelativeRoot {
		t.Fatalf("got status=%v reason=%v, want unknown/relative_root", res.Status, res.Reason)
	}
}

// --- F13: the port name is ONE segment under the root --------------------

// TestLastFailure_PortNameEscapingBuildtreesRoot_Refused: `..\outside` would
// make the tool read and report logs from OUTSIDE the root the caller
// granted it.
func TestLastFailure_PortNameEscapingBuildtreesRoot_Refused(t *testing.T) {
	root := absPath(t, "testdata/failing_port/buildtrees")
	for _, port := range []string{
		`..\outside`,
		"../outside",
		"sub/dir",
		"..",
		".",
		"Upper-Case", // not a legal vcpkg port name: lowercase only
		"-leading",
		"trailing-",
	} {
		res := LastFailure(Args{Port: port, BuildtreesRoot: root}, testDeps())
		if res.Status != evidence.StatusUnknown || res.Reason != ReasonInvalidPortName {
			t.Errorf("port %q: got status=%v reason=%v, want unknown/invalid_port_name",
				port, res.Status, res.Reason)
		}
	}

	// A legal hyphenated port name must still be accepted.
	ok := LastFailure(Args{
		Port:           "boost-algorithm",
		Triplet:        "wingpl",
		BuildtreesRoot: absPath(t, "testdata/wingpl_like/buildtrees"),
	}, testDeps())
	if ok.Reason == ReasonInvalidPortName {
		t.Errorf("legal port name boost-algorithm was rejected: %+v", ok)
	}
}

// --- F10: filesystem errors are not absence ------------------------------

// TestLastFailure_BuildtreesRootUnreadable_NotReportedAsCleaned: an
// access-denied probe is not evidence that --clean-buildtrees-after-build
// removed the tree.
func TestLastFailure_BuildtreesRootUnreadable_NotReportedAsCleaned(t *testing.T) {
	deps := testDeps()
	root := absPath(t, "testdata/failing_port/buildtrees")
	deps.FS = failingFS{inner: DefaultFS(), failSub: "failing_port/buildtrees", failStat: true}

	res := LastFailure(Args{Port: "somelib", BuildtreesRoot: root}, deps)
	if res.Reason == ReasonBuildtreesRootAbsent {
		t.Fatal("an unreadable buildtrees root was reported as absent — " +
			"that is a VERIFIED-absence claim manufactured from a failure to look")
	}
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonBuildtreesRootUnreadable {
		t.Fatalf("got status=%v reason=%v, want unknown/buildtrees_root_unreadable", res.Status, res.Reason)
	}
}

// TestLastFailure_PortDirUnreadable_NotReportedAsNotFound: same rule one
// level down.
func TestLastFailure_PortDirUnreadable_NotReportedAsNotFound(t *testing.T) {
	deps := testDeps()
	root := absPath(t, "testdata/failing_port/buildtrees")
	deps.FS = failingFS{inner: DefaultFS(), failSub: "buildtrees/somelib", failStat: true}

	res := LastFailure(Args{Port: "somelib", BuildtreesRoot: root}, deps)
	if res.Reason == ReasonPortDirNotFound {
		t.Fatal("an unreadable port directory was reported as port_dir_not_found")
	}
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPortDirUnreadable {
		t.Fatalf("got status=%v reason=%v, want unknown/port_dir_unreadable", res.Status, res.Reason)
	}
}

// --- F9: an unread log can change the verdict ----------------------------

// TestLastFailure_UnreadablePhaseLog_NoConfidentVerdict: somelib has a real
// install-phase error, but one of its logs cannot be read. That log could
// hold a later-phase error, the only error, or an interrupt marker, so the
// confident `failed` verdict may not be issued.
func TestLastFailure_UnreadablePhaseLog_NoConfidentVerdict(t *testing.T) {
	root := absPath(t, "testdata/failing_port/buildtrees")

	// Baseline with a fully readable tree, so the ONLY difference below is
	// the readability of one log.
	base := LastFailure(Args{Port: "somelib", BuildtreesRoot: root}, testDeps())
	if base.Status != evidence.StatusFailed {
		t.Fatalf("baseline status = %v, want failed; result=%+v", base.Status, base)
	}

	deps := testDeps()
	deps.FS = failingFS{inner: DefaultFS(), failSub: "somelib/config-cl-out.log", failRead: true}
	res := LastFailure(Args{Port: "somelib", BuildtreesRoot: root}, deps)

	if res.Status == evidence.StatusFailed {
		t.Fatal("a confident `failed` verdict was issued while a relevant phase log was unreadable")
	}
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPhaseLogUnreadable {
		t.Fatalf("got status=%v reason=%v, want unknown/phase_log_unreadable", res.Status, res.Reason)
	}
	// The readable evidence must still be returned, not withheld.
	if len(res.Diagnostics) == 0 {
		t.Error("diagnostics recovered from the READABLE logs must still be surfaced")
	}
	if len(res.LogPaths) == 0 {
		t.Error("log_paths must still be populated")
	}
}

// --- F6: warnings are not failures ---------------------------------------

// TestLastFailure_WarningOnlyLog_NotAFailure: a log whose only matches are
// warnings is the normal state of a SUCCESSFUL C++ build.
func TestLastFailure_WarningOnlyLog_NotAFailure(t *testing.T) {
	root := writePortLogs(t, "warnlib", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"install-cl-rel-out.log": "foo.cpp:1:1: warning: 'x' is deprecated\n" +
			`bar.cpp(7): warning C4244: 'initializing': possible loss of data` + "\n",
	})

	res := LastFailure(Args{Port: "warnlib", Triplet: "cl", BuildtreesRoot: root}, testDeps())
	if res.Status == evidence.StatusFailed {
		t.Fatalf("a warning-only log was reported as a build FAILURE: %+v", res)
	}
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoFailureDiagnostic {
		t.Fatalf("got status=%v reason=%v, want unknown/no_failure_diagnostic", res.Status, res.Reason)
	}
	// The warnings themselves are still evidence and must be returned.
	if len(res.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %+v, want both warnings surfaced as evidence", res.Diagnostics)
	}
	for _, d := range res.Diagnostics {
		if d.Severity != "warning" {
			t.Errorf("diagnostic %+v: severity must stay warning", d)
		}
	}
}

// TestLastFailure_ErrorOutranksLaterPhaseWarning: phase SELECTION keys on
// error severity, so a later phase's warning cannot displace a real error
// found in an earlier phase.
func TestLastFailure_ErrorOutranksLaterPhaseWarning(t *testing.T) {
	root := writePortLogs(t, "mixedlib", map[string]string{
		"cl.vcpkg_abi_info.txt":  "abi\n",
		"config-cl-out.log":      `cfg.cpp(3): error C2065: 'boom': undeclared identifier` + "\n",
		"install-cl-rel-out.log": `later.cpp(9): warning C4244: possible loss of data` + "\n",
	})

	res := LastFailure(Args{Port: "mixedlib", Triplet: "cl", BuildtreesRoot: root}, testDeps())
	if res.Status != evidence.StatusFailed {
		t.Fatalf("status = %v, want failed (a real config-phase error exists); result=%+v", res.Status, res)
	}
	if res.Phase != PhaseConfig {
		t.Fatalf("phase = %q, want config — a later-phase WARNING must not displace an earlier real error", res.Phase)
	}
}

// --- F5: an incomplete wrapper list proves nothing -----------------------

// TestLastFailure_IncompleteWrapperList_NeverConfirmsNoFailure: the wrapper
// declares 2 failures but lists 1. Querying the OMITTED port must not yield
// a confident ok telling the operator to stop looking at a port that failed.
func TestLastFailure_IncompleteWrapperList_NeverConfirmsNoFailure(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "build_failed.log")
	content := "[2026-07-24 21:35:11] triplet=cl\n" +
		"command: vcpkg.exe install foo bar --triplet=cl\n" +
		"exit_code: 1\n" +
		"build_failed_count: 2\n" +
		"failed_ports:\n" +
		"- foo:cl\n"
	if err := os.WriteFile(wrapper, []byte(content), 0o644); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	res := LastFailure(Args{
		Port:           "bar", // the OMITTED port
		Triplet:        "cl",
		BuildFailedLog: wrapper,
		BuildtreesRoot: absPath(t, "testdata/failing_port/buildtrees"),
	}, testDeps())

	if res.Status == evidence.StatusOK && containsNote(res.Notes, NoteWrapperConfirmsNoFailure) {
		t.Fatal("an INCOMPLETE wrapper list (build_failed_count=2, 1 entry) was used to PROVE " +
			"the omitted port did not fail — its silence is not evidence")
	}
	if !containsNote(res.Notes, NoteWrapperFailedPortsCompletenessUnproven) {
		t.Errorf("notes = %v, want wrapper_failed_ports_list_completeness_unproven recorded", res.Notes)
	}
}

// TestLastFailure_CompleteWrapperListStillConfirms: the negative
// confirmation is preserved when the list IS provably exhaustive.
func TestLastFailure_CompleteWrapperListStillConfirms(t *testing.T) {
	res := LastFailure(Args{
		Port:           "somelib",
		BuildFailedLog: "testdata/wrapper_confirms_no_failure.log", // count 1, 1 entry
	}, testDeps())
	if res.Status != evidence.StatusOK || !containsNote(res.Notes, NoteWrapperConfirmsNoFailure) {
		t.Fatalf("a PROVABLY complete wrapper list must still confirm; got %+v", res)
	}
}

// TestWrapperInfo_FailedPortsListIsComplete covers the three independent
// conditions of the completeness guard.
func TestWrapperInfo_FailedPortsListIsComplete(t *testing.T) {
	one, two := 1, 2
	cases := []struct {
		name string
		info WrapperInfo
		want bool
	}{
		{"complete", WrapperInfo{ScanComplete: true, BuildFailedCount: &one, FailedPorts: []string{"a:cl"}}, true},
		{"count_mismatch", WrapperInfo{ScanComplete: true, BuildFailedCount: &two, FailedPorts: []string{"a:cl"}}, false},
		{"no_count_declared", WrapperInfo{ScanComplete: true, FailedPorts: []string{"a:cl"}}, false},
		{"scan_incomplete", WrapperInfo{ScanComplete: false, BuildFailedCount: &one, FailedPorts: []string{"a:cl"}}, false},
	}
	for _, tc := range cases {
		if got := tc.info.FailedPortsListIsComplete(); got != tc.want {
			t.Errorf("%s: FailedPortsListIsComplete() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParseWrapperContent_ScanErrorSurfaced: a line longer than the scanner
// buffer truncates the failed_ports list silently unless the error is
// checked.
func TestParseWrapperContent_ScanErrorSurfaced(t *testing.T) {
	huge := strings.Repeat("x", 5*1024*1024)
	data := []byte("[2026-07-24 21:35:11] triplet=cl\n" +
		"build_failed_count: 1\n" +
		"failed_ports:\n" +
		huge + "\n" +
		"- foo:cl\n")

	info, ok, err := ParseWrapperContent(data)
	if err == nil {
		t.Fatal("expected a scanner error for an over-long line")
	}
	if !ok {
		t.Error("partial context recovered before the error is still usable (ok must stay true)")
	}
	if info.ScanComplete {
		t.Error("ScanComplete must be false after a scan error")
	}
	if info.FailedPortsListIsComplete() {
		t.Error("a truncated scan must never report an exhaustive failed_ports list")
	}
}

// --- F12: quote-aware command-line parsing -------------------------------

// TestSplitWindowsCommandLine_MatchesRealCommandLineToArgvW: every
// expectation below was captured from the REAL shell32 CommandLineToArgvW on
// Windows 11 during this change, not derived from documentation alone.
func TestSplitWindowsCommandLine_MatchesRealCommandLineToArgvW(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{
			`vcpkg.exe install --x-buildtrees-root="D:\vcpkg builds" --triplet=cl`,
			[]string{`vcpkg.exe`, `install`, `--x-buildtrees-root=D:\vcpkg builds`, `--triplet=cl`},
		},
		{
			`vcpkg.exe --x-buildtrees-root "D:\vcpkg builds" --overlay-ports="C:\a b\ports"`,
			[]string{`vcpkg.exe`, `--x-buildtrees-root`, `D:\vcpkg builds`, `--overlay-ports=C:\a b\ports`},
		},
		{`a.exe "q\\" x" plain`, []string{`a.exe`, `q\`, `x plain`}},
		{`a.exe c:\path\\ "d:\p 2\\" tail`, []string{`a.exe`, `c:\path\\`, `d:\p 2\`, `tail`}},
		{`a.exe --k="v""w" --z`, []string{`a.exe`, `--k=v"w --z`}},
		{`a.exe "x""y z"`, []string{`a.exe`, `x"y`, `z`}},
		{`a.exe --k="a\"b" tail`, []string{`a.exe`, `--k=a"b`, `tail`}},
		{`a.exe   --triplet   cl    --x-install-root=q:/v`, []string{`a.exe`, `--triplet`, `cl`, `--x-install-root=q:/v`}},
	}
	for _, tc := range cases {
		got := SplitWindowsCommandLine(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s\n got  %q\n want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s\n arg[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// TestParseWrapperContent_QuotedRootWithSpaces is the regression the
// quote-aware parse exists for: the previous `\S+` pattern truncated this at
// the space and yielded `"D:\vcpkg` — a path that does not exist, which the
// buildtrees probe then reported as a cleaned tree.
func TestParseWrapperContent_QuotedRootWithSpaces(t *testing.T) {
	data := []byte("[2026-07-24 21:35:11] triplet=cl\n" +
		`command: C:\vcpkg\vcpkg.exe install foo --x-buildtrees-root="D:\vcpkg builds" ` +
		`--overlay-ports="C:\my overlays\ports" --overlay-ports=C:\plain\ports ` +
		`--x-install-root "Q:\install root" --triplet=cl` + "\n")

	info, ok, err := ParseWrapperContent(data)
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if info.BuildtreesRoot != `D:\vcpkg builds` {
		t.Errorf("BuildtreesRoot = %q, want %q (a quoted path must survive its spaces intact)",
			info.BuildtreesRoot, `D:\vcpkg builds`)
	}
	if info.InstallRoot != `Q:\install root` {
		t.Errorf("InstallRoot = %q, want %q (space-separated --key value form)",
			info.InstallRoot, `Q:\install root`)
	}
	want := []string{`C:\my overlays\ports`, `C:\plain\ports`}
	if len(info.OverlayPorts) != 2 || info.OverlayPorts[0] != want[0] || info.OverlayPorts[1] != want[1] {
		t.Errorf("OverlayPorts = %q, want %q in precedence order", info.OverlayPorts, want)
	}
}

// --- F11: production must read the real environment ----------------------

// TestDefaultDeps_GetenvReadsRealEnvironment: the injection seam must not
// silently blind production. resolveOverlayChain consults
// VCPKG_OVERLAY_PORTS; with a stubbed Getenv the tool reported
// overlay_chain_none_builtin_ports_only — a positive claim made without ever
// reading the variable that contradicts it.
func TestDefaultDeps_GetenvReadsRealEnvironment(t *testing.T) {
	const key = "MCPHUB_TEST_LASTFAILURE_GETENV_PROBE"
	t.Setenv(key, "sentinel")
	deps := DefaultDeps()
	if deps.Getenv == nil {
		t.Fatal("DefaultDeps().Getenv is nil")
	}
	if got := deps.Getenv(key); got != "sentinel" {
		t.Fatalf("DefaultDeps().Getenv(%q) = %q, want \"sentinel\" — "+
			"production must read the real environment", key, got)
	}
}

// TestLastFailure_OverlayChainFromEnv is the end-to-end consequence.
func TestLastFailure_OverlayChainFromEnv(t *testing.T) {
	deps := testDeps()
	deps.Getenv = func(k string) string {
		if k == "VCPKG_OVERLAY_PORTS" {
			return "overlay-a"
		}
		return ""
	}
	res := LastFailure(Args{
		Port:           "somelib",
		BuildtreesRoot: absPath(t, "testdata/failing_port/buildtrees"),
	}, deps)
	if !containsNote(res.Notes, NoteOverlayChainFromEnv) {
		t.Fatalf("notes = %v, want overlay_chain_from_env when VCPKG_OVERLAY_PORTS is set", res.Notes)
	}
	if len(res.OverlayChain) == 0 {
		t.Error("overlay_chain must carry the env-supplied overlay")
	}
}
