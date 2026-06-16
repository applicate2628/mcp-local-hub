package vtune

import (
	"strings"
	"testing"
)

// TestParseCollectList extracts the analysis-type ids from a representative
// `vtune -collect-list` body (non-indented id line + indented description
// continuation), skipping the header and the indented description lines, and
// de-duplicating. This is a PURE parse — it never spawns vtune.
func TestParseCollectList(t *testing.T) {
	raw := "Available analysis types:\n" +
		"hotspots\n" +
		"    Analyze application flow and identify sections of code that take a\n" +
		"    long time to execute (hotspots).\n" +
		"memory-access\n" +
		"    Analyze memory accesses to find low-efficiency hotspots.\n" +
		"threading\n" +
		"    Analyze CPU utilization and thread parallelism.\n" +
		"uarch-exploration\n" +
		"    Analyze CPU microarchitecture usage.\n" +
		"hotspots\n" + // duplicate must be collapsed
		"    (repeat — de-dup guard)\n"

	got := parseCollectList(raw)
	want := []string{"hotspots", "memory-access", "threading", "uarch-exploration"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("parseCollectList = %v, want %v", got, want)
	}
}

// TestParseCollectList_TolerantOnGarbage verifies the parser never panics and
// degrades to "fewer ids" rather than failing when handed an unfamiliar /
// empty body: empty input yields no ids, and a header-only body with no
// recognizable id lines yields none either.
func TestParseCollectList_TolerantOnGarbage(t *testing.T) {
	if got := parseCollectList(""); len(got) != 0 {
		t.Errorf("parseCollectList(\"\") = %v, want empty", got)
	}
	// A header line followed by only indented prose — no id lines.
	raw := "Some unexpected banner with Capitals And Spaces\n" +
		"    indented continuation only\n"
	if got := parseCollectList(raw); len(got) != 0 {
		t.Errorf("parseCollectList(no-ids) = %v, want empty (Capitalized header is not an id)", got)
	}
}

// TestLooksLikeAnalysisID accepts the VTune id shape (lowercase / digits /
// hyphen) and rejects capitalized headers, empties, and punctuation.
func TestLooksLikeAnalysisID(t *testing.T) {
	for _, ok := range []string{"hotspots", "memory-access", "uarch-exploration", "io", "gpu-offload"} {
		if !looksLikeAnalysisID(ok) {
			t.Errorf("looksLikeAnalysisID(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "Available", "Hotspots", "memory_access", "a b", "what?"} {
		if looksLikeAnalysisID(bad) {
			t.Errorf("looksLikeAnalysisID(%q) = true, want false", bad)
		}
	}
}

// TestSepDriverNote always returns the load-bearing operator guidance and, when
// the version banner carries a driver/sampling status line, prepends it.
func TestSepDriverNote(t *testing.T) {
	// No banner: generic guidance only, never empty.
	bare := sepDriverNote("")
	if bare == "" {
		t.Fatal("sepDriverNote(\"\") is empty; want generic guidance")
	}
	if !strings.Contains(bare, "hotspots") || !strings.Contains(bare, "SEP") {
		t.Errorf("sepDriverNote(\"\") = %q, want to mention hotspots + SEP guidance", bare)
	}

	// Banner with a driver line: it must be surfaced ahead of the guidance.
	withDriver := sepDriverNote("Intel(R) VTune(TM) Profiler 2026.2.0\nSampling Driver: not installed\n")
	if !strings.Contains(withDriver, "Sampling Driver: not installed") {
		t.Errorf("sepDriverNote did not surface the driver status line: %q", withDriver)
	}
	if !strings.Contains(withDriver, "SEP") {
		t.Errorf("sepDriverNote dropped the generic guidance: %q", withDriver)
	}
}

// TestFirstNonEmptyLine returns the first non-blank line, trimmed.
func TestFirstNonEmptyLine(t *testing.T) {
	in := "\n  \nIntel(R) VTune(TM) Profiler 2026.2.0  \nbuild 123\n"
	if got := firstNonEmptyLine(in); got != "Intel(R) VTune(TM) Profiler 2026.2.0" {
		t.Errorf("firstNonEmptyLine = %q, want the trimmed version line", got)
	}
	if got := firstNonEmptyLine(""); got != "" {
		t.Errorf("firstNonEmptyLine(\"\") = %q, want empty", got)
	}
}
