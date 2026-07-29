package lastfailure

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type syntheticDirEntry string

func (e syntheticDirEntry) Name() string             { return string(e) }
func (syntheticDirEntry) IsDir() bool                { return false }
func (syntheticDirEntry) Type() os.FileMode          { return 0 }
func (syntheticDirEntry) Info() (os.FileInfo, error) { return nil, nil }

type recordingDirReader struct {
	entries  []os.DirEntry
	requests []int
	closed   bool
}

func (r *recordingDirReader) ReadDir(n int) ([]os.DirEntry, error) {
	r.requests = append(r.requests, n)
	if len(r.entries) == 0 {
		return nil, io.EOF
	}
	if n > len(r.entries) {
		n = len(r.entries)
	}
	out := r.entries[:n]
	r.entries = r.entries[n:]
	return out, nil
}
func (r *recordingDirReader) Close() error { r.closed = true; return nil }

type pagingProbeFS struct {
	FS
	path     string
	reader   *recordingDirReader
	logOpens int
}

func (f *pagingProbeFS) OpenDir(path string) (DirReader, error) {
	if path == f.path {
		return f.reader, nil
	}
	return f.FS.OpenDir(path)
}
func (f *pagingProbeFS) Open(path string) (io.ReadCloser, error) {
	f.logOpens++
	return f.FS.Open(path)
}

func TestLastFailure_PortDirEntryLimitFailsClosed(t *testing.T) {
	root := t.TempDir()
	portDir := filepath.Join(root, "many")
	if err := os.Mkdir(portDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := make([]os.DirEntry, defaultResponseLimits.directoryEntries+1)
	for i := range entries {
		entries[i] = syntheticDirEntry("artifact-" + strings.Repeat("x", i%7))
	}
	reader := &recordingDirReader{entries: entries}
	fsys := &pagingProbeFS{FS: DefaultFS(), path: portDir, reader: reader}
	res := LastFailure(Args{Port: "many", Triplet: "cl", BuildtreesRoot: root},
		Deps{FS: fsys, Getenv: func(string) string { return "" }})
	if res.Status != Status("unknown") || res.Reason != ReasonArtifactLimitExceeded {
		t.Fatalf("status=%s reason=%s, want unknown(%s)", res.Status, res.Reason, ReasonArtifactLimitExceeded)
	}
	if len(reader.requests) != 1 || reader.requests[0] != defaultResponseLimits.directoryEntries+1 {
		t.Fatalf("directory requests=%v, want one page of limit+sentinel=%d", reader.requests, defaultResponseLimits.directoryEntries+1)
	}
	if !reader.closed || fsys.logOpens != 0 {
		t.Fatalf("closed=%v log opens=%d, want closed/0", reader.closed, fsys.logOpens)
	}
	if res.Resources.Completeness.DirectoryEntries || res.Resources.Omitted.DirectoryEntriesAtLeast < 1 {
		t.Fatalf("resource report=%+v", res.Resources)
	}
}

func TestLastFailure_RelevantLogCountLimitFailsClosed(t *testing.T) {
	root := t.TempDir()
	portDir := filepath.Join(root, "manylogs")
	if err := os.Mkdir(portDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < defaultResponseLimits.relevantLogs+1; i++ {
		name := filepath.Join(portDir, "install-cl-cfg"+strings.Repeat("x", i)+"-out.log")
		if err := os.WriteFile(name, []byte("ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fsys := &pagingProbeFS{FS: DefaultFS(), path: "never"}
	res := LastFailure(Args{Port: "manylogs", Triplet: "cl", BuildtreesRoot: root},
		Deps{FS: fsys, Getenv: func(string) string { return "" }})
	if res.Status != Status("unknown") || res.Reason != ReasonArtifactLimitExceeded || fsys.logOpens != 0 {
		t.Fatalf("status=%s reason=%s logOpens=%d", res.Status, res.Reason, fsys.logOpens)
	}
	if res.Resources.Completeness.RelevantLogs || res.Resources.HighWater.RelevantLogs > defaultResponseLimits.relevantLogs {
		t.Fatalf("resource report=%+v", res.Resources)
	}
}

func TestLastFailure_TotalLogByteLimitFailsClosed(t *testing.T) {
	root := t.TempDir()
	portDir := filepath.Join(root, "bytes")
	if err := os.Mkdir(portDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(portDir, "install-cl-rel-err.log"), []byte(strings.Repeat("x", 40)), 0o644); err != nil {
		t.Fatal(err)
	}
	limits := defaultResponseLimits
	limits.totalLogBytes, limits.logBytes = 16, 64
	res := lastFailureWithLimits(context.Background(), Args{Port: "bytes", Triplet: "cl", BuildtreesRoot: root},
		Deps{FS: DefaultFS(), Getenv: func(string) string { return "" }}, limits)
	if res.Status != Status("unknown") || res.Reason != ReasonArtifactLimitExceeded {
		t.Fatalf("status=%s reason=%s, want unknown(%s)", res.Status, res.Reason, ReasonArtifactLimitExceeded)
	}
	if res.Resources.Completeness.LogBytes || res.Resources.HighWater.LogBytes != limits.totalLogBytes {
		t.Fatalf("resource report=%+v", res.Resources)
	}
}

func TestLastFailure_WrapperMetadataLimitFailsClosed(t *testing.T) {
	limits := defaultResponseLimits
	limits.metadataBytes = 32
	const path = "wrapper.log"
	res := lastFailureWithLimits(context.Background(), Args{BuildFailedLog: path},
		Deps{FS: oneFileFS{path: path, data: []byte(strings.Repeat("x", 64))}}, limits)
	if res.Status != Status("unknown") || res.Reason != ReasonMetadataLimitExceeded {
		t.Fatalf("status=%s reason=%s", res.Status, res.Reason)
	}
	if res.Resources.Completeness.Metadata || res.Resources.Omitted.WrapperBytesAtLeast < 1 {
		t.Fatalf("resource report=%+v", res.Resources)
	}
}

func TestLastFailure_OverlayAndFailedPortListsAreBoundedAndReported(t *testing.T) {
	limits := defaultResponseLimits
	limits.overlayEntries, limits.listEntries = 2, 2
	data := []byte("command: vcpkg install --overlay-ports=a --overlay-ports=b --overlay-ports=c\n" +
		"build_failed_count: 3\nfailed_ports:\n- a:cl\n- b:cl\n- c:cl\n")
	info, ok, err := parseWrapperContentWithLimits(data, limits)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(info.OverlayPorts) != 2 || info.OverlayPortsDropped != 1 || len(info.FailedPorts) != 2 || info.FailedPortsDropped != 1 {
		t.Fatalf("bounded wrapper=%+v", info)
	}
	if info.FailedPortsListIsComplete() {
		t.Fatal("a dropped failed-port entry must disqualify exhaustive inference")
	}
}

func TestLastFailure_ProducerAggregationCapsBeforeResultMaterialization(t *testing.T) {
	accumulator := newDiagnosticAccumulator(2)
	var diagnostics []Diagnostic
	for _, severity := range []string{SeverityError, "warning"} {
		for _, tier := range []DiagnosticTier{TierSpecific, TierAggregate} {
			for i := 0; i < 5; i++ {
				diagnostics = append(diagnostics, Diagnostic{Severity: severity, Tier: tier, Text: severity + string(tier)})
			}
		}
	}
	for _, diagnostic := range diagnostics {
		accumulator.addDiagnostic(phaseLogFile{Phase: PhaseBuild, Path: "log"}, diagnostic)
	}
	ranked := accumulator.ranked()
	if accumulator.highWater != 8 || len(ranked) != 8 || accumulator.dropped != 12 {
		t.Fatalf("highWater=%d retained=%d dropped=%d, want 8/8/12", accumulator.highWater, len(ranked), accumulator.dropped)
	}
	if ranked[0].diagnostic.Severity != SeverityError || ranked[0].diagnostic.Tier != TierSpecific {
		t.Fatalf("first candidate=%+v, stable severity/tier order lost", ranked[0])
	}
}

func TestResponseBudget_FailedCausalTupleSurvivesEveryReductionStage(t *testing.T) {
	diagnostic := Diagnostic{File: "a.cpp", Line: 1, Severity: SeverityError, Tier: TierSpecific, Text: "a.cpp:1: error: boom"}
	input := Result{Status: Status("failed"), Phase: PhaseBuild, FirstError: &diagnostic,
		Diagnostics: []Diagnostic{diagnostic, diagnostic}, DiagnosticLog: "build.log",
		LogPaths: []string{"other.log", "build.log"}, ExitCode: intPtr(1),
		Resources: completeResourceReport(), DiagnosticsDroppedExact: true}
	for name, result := range map[string]Result{"normal": boundResponse(input), "reduced": reduceOversizeResponse(input)} {
		if result.Status != Status("failed") || result.FirstError == nil || len(result.Diagnostics) == 0 ||
			result.Diagnostics[0] != *result.FirstError || result.DiagnosticLog == "" || result.LogPaths[0] != result.DiagnosticLog {
			t.Fatalf("%s lost causal tuple: %+v", name, result)
		}
	}
}

func TestResponseBudget_MinimalFallbackCannotEmitUncausedFailure(t *testing.T) {
	result := validateCausality(Result{Status: Status("failed"), Phase: PhaseBuild})
	if result.Status != Status("unknown") || result.Reason != ReasonCausalityInvariantViolation {
		t.Fatalf("status=%s reason=%s", result.Status, result.Reason)
	}
}

func TestTruncateWireValue_TinyBudgetTerminatesAndStaysBounded(t *testing.T) {
	input := strings.Repeat("x", 64)
	for max := 0; max < len(fmt.Sprintf(truncationMarker, len(input))); max++ {
		value, truncated := truncateWireValue(input, max)
		if !truncated || len(value) > max {
			t.Fatalf("max=%d value=%q bytes=%d truncated=%v", max, value, len(value), truncated)
		}
	}
}

func intPtr(value int) *int { return &value }

func BenchmarkLastFailure_PathologicalBoundedAggregation(b *testing.B) {
	root := b.TempDir()
	portDir := filepath.Join(root, "bench")
	if err := os.Mkdir(portDir, 0o755); err != nil {
		b.Fatal(err)
	}
	log := strings.Repeat("a.cpp:1:1: error: bounded diagnostic\n", 300)
	for i := 0; i < 32; i++ {
		for _, stream := range []string{"out", "err"} {
			name := filepath.Join(portDir, "install-cl-cfg"+strings.Repeat("x", i)+"-"+stream+".log")
			if err := os.WriteFile(name, []byte(log), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	args := Args{Port: "bench", Triplet: "cl", BuildtreesRoot: root}
	deps := Deps{FS: DefaultFS(), Getenv: func(string) string { return "" }}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := LastFailure(args, deps)
		if result.Status != Status("failed") {
			b.Fatalf("status=%s reason=%s", result.Status, result.Reason)
		}
		b.ReportMetric(float64(result.Resources.HighWater.DiagnosticCandidates), "candidates_peak")
	}
}
