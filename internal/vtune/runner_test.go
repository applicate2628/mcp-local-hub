package vtune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/oneapi"
)

// TestBuildCollectArgs asserts the collect-phase flag wiring without spawning
// a process: -collect <analysis>, -r <resultdir>, the "--" separator, then the
// target + its args in order.
func TestBuildCollectArgs(t *testing.T) {
	tests := []struct {
		name      string
		analysis  string
		resultDir string
		target    string
		args      []string
		want      []string
	}{
		{
			name:      "hotspots-no-args",
			analysis:  "hotspots",
			resultDir: "RES",
			target:    "t.exe",
			args:      nil,
			want:      []string{"-collect", "hotspots", "-r", "RES", "--", "t.exe"},
		},
		{
			name:      "threading-with-args",
			analysis:  "threading",
			resultDir: "RES",
			target:    "t.exe",
			args:      []string{"-a", "-b"},
			want:      []string{"-collect", "threading", "-r", "RES", "--", "t.exe", "-a", "-b"},
		},
		{
			name:      "memory-access",
			analysis:  "memory-access",
			resultDir: `C:\tmp\result`,
			target:    `C:\proj\app.exe`,
			args:      []string{"--input", "data.bin"},
			want:      []string{"-collect", "memory-access", "-r", `C:\tmp\result`, "--", `C:\proj\app.exe`, "--input", "data.bin"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCollectArgs(tc.analysis, tc.resultDir, tc.target, tc.args)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("buildCollectArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildReportArgs asserts the report-phase flag wiring: -report <name>,
// -r <resultdir>, -format=csv, and -report-output <file> (LOAD-BEARING — it
// keeps VTune's progress chatter out of the report body).
func TestBuildReportArgs(t *testing.T) {
	got := buildReportArgs("hotspots", "RES", "OUT.csv")
	want := []string{"-report", "hotspots", "-r", "RES", "-format=csv", "-report-output", "OUT.csv"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("buildReportArgs = %v, want %v", got, want)
	}

	// summary report uses the same shape with the "summary" name.
	gotSum := buildReportArgs("summary", "RES", "SUM.txt")
	wantSum := []string{"-report", "summary", "-r", "RES", "-format=csv", "-report-output", "SUM.txt"}
	if strings.Join(gotSum, "|") != strings.Join(wantSum, "|") {
		t.Errorf("buildReportArgs(summary) = %v, want %v", gotSum, wantSum)
	}
}

// TestKnownAnalysisTypes guards the allowlist: every type named in the tool
// contract is accepted, and an obvious unknown is not. reportName maps EVERY
// accepted analysis type to the universal "hotspots" report name — VTune's
// -report names are a fixed set that does NOT include the collect analysis
// types, so the table report must be a real report name (verified live).
func TestKnownAnalysisTypes(t *testing.T) {
	for _, want := range []string{"hotspots", "memory-access", "threading", "uarch-exploration", "memory-consumption"} {
		if !knownAnalysisTypes[want] {
			t.Errorf("knownAnalysisTypes missing %q", want)
		}
		if got := reportName(want); got != "hotspots" {
			t.Errorf("reportName(%q) = %q, want \"hotspots\" (a valid VTune -report name)", want, got)
		}
	}
	if knownAnalysisTypes["gpu-hotspots"] {
		t.Error("knownAnalysisTypes must NOT include the unvalidated 'gpu-hotspots'")
	}
	if defaultAnalysisType != "hotspots" {
		t.Errorf("defaultAnalysisType = %q, want hotspots", defaultAnalysisType)
	}
}

// TestVTuneEnv_FallsBackToOSEnvWhenNoOneAPI verifies the vtuneEnv fallback
// chain: when setvars cannot be captured AND DetectRoot finds no oneAPI root
// (forced via the oneapi test seam returning dirExists=false for every probe),
// vtuneEnv returns os.Environ() unchanged — never nil, never empty. This is
// the host-independent fallback the env primitive guarantees.
func TestVTuneEnv_FallsBackToOSEnvWhenNoOneAPI(t *testing.T) {
	// Force DetectRoot() to find nothing: dirExists is false for every probe,
	// so SetvarsEnv (which calls DetectRoot internally) also yields nothing.
	restore := oneapi.SetSeamsForTest(
		func(string) bool { return false }, // dirExists → no root anywhere
		nil,
		nil,
	)
	defer restore()

	got := vtuneEnv()
	if len(got) == 0 {
		t.Fatal("vtuneEnv returned empty env on the no-oneAPI fallback; want os.Environ()")
	}
	// It must be the process environment (a stable, always-present var like the
	// one Go's test harness exports). Compare lengths as a sanity check that we
	// returned os.Environ() rather than a synthesized slice.
	if len(got) != len(os.Environ()) {
		t.Errorf("vtuneEnv fallback len = %d, want os.Environ() len %d", len(got), len(os.Environ()))
	}
}

// TestReadReportFile reads a present file (capped) and returns "" for a
// missing one.
func TestReadReportFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "report.csv")
	if err := os.WriteFile(f, []byte("Function\tCPU Time\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readReportFile(f); !strings.Contains(got, "Function") {
		t.Errorf("readReportFile = %q, want the file body", got)
	}
	if got := readReportFile(filepath.Join(dir, "absent.csv")); got != "" {
		t.Errorf("readReportFile(missing) = %q, want empty", got)
	}
}

// TestReportPathIfPresent returns the path only when the file exists.
func TestReportPathIfPresent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "report.csv")
	if got := reportPathIfPresent(f); got != "" {
		t.Errorf("reportPathIfPresent(missing) = %q, want empty", got)
	}
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := reportPathIfPresent(f); got != f {
		t.Errorf("reportPathIfPresent(present) = %q, want %q", got, f)
	}
}

// TestFormatCommandLine quotes space-bearing tokens (the real vtune.exe lives
// under "C:\Program Files (x86)\Intel\oneAPI\vtune\latest\bin64\") so the
// surfaced command line stays one pasteable token per arg.
func TestFormatCommandLine(t *testing.T) {
	exe := `C:\Program Files (x86)\Intel\oneAPI\vtune\latest\bin64\vtune.exe`
	args := []string{"-collect", "hotspots", "-r", `C:\tmp\result`, "--", `C:\proj\app.exe`, "--flag"}
	got := formatCommandLine(exe, args)

	if !strings.Contains(got, `"`+exe+`"`) {
		t.Errorf("command line did not quote the space-bearing exe path: %s", got)
	}
	if strings.Contains(got, `"-collect"`) {
		t.Errorf("space-free arg was needlessly quoted: %s", got)
	}
	for _, want := range append([]string{exe}, args...) {
		if !strings.Contains(got, want) {
			t.Errorf("command line missing token %q: %s", want, got)
		}
	}
}

// TestTruncate caps oversized bodies with a visible marker.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", 50)
	got := truncate(long, 10)
	if len(got) <= 10 || !strings.Contains(got, "[truncated]") {
		t.Errorf("truncate did not mark clipped output: %q", got)
	}
}

func TestVtuneEnabledRequiresExplicitOptIn(t *testing.T) {
	t.Setenv(enableUnsafeVtuneEnv, "")
	if vtuneEnabled() {
		t.Fatal("vtune should be disabled without explicit opt-in")
	}
	t.Setenv(enableUnsafeVtuneEnv, "1")
	if !vtuneEnabled() {
		t.Fatal("vtune should be enabled with opt-in =1")
	}
}
