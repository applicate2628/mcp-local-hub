package cmakegraph

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestR15AggregateEdgeCountCapReportsCoverage(t *testing.T) {
	dir := t.TempDir()
	var source strings.Builder
	for i := 0; i < 20001; i++ {
		fmt.Fprintf(&source, "include(missing-%05d.cmake)\n", i)
	}
	root := writeFile(t, dir, "CMakeLists.txt", source.String())

	result, err := Walk(context.Background(), root, dir, Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(result.Edges) >= 20001 {
		t.Fatalf("retained %d edges from 20001 calls; aggregate edge count is unbounded", len(result.Edges))
	}
	assertCoverageReason(t, result, "edge_cap_exceeded")
}

func TestR15AggregateEdgeByteCapReportsCoverage(t *testing.T) {
	dir := t.TempDir()
	largeArg := strings.Repeat("a", 3<<20) + ".cmake"
	for i := 0; i < 3; i++ {
		writeFile(t, dir, filepath.Join(fmt.Sprintf("p%d", i), "CMakeLists.txt"), "include(\""+largeArg+"\")\n")
	}

	result, err := WalkTree(context.Background(), dir, dir, []string{"CMakeLists.txt"}, Options{})
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	if len(result.Edges) >= 3 {
		t.Fatalf("retained all %d large edges; aggregate retained bytes are unbounded", len(result.Edges))
	}
	assertCoverageReason(t, result, "edge_cap_exceeded")
}

func TestR15MissingOptionalIncludeIsNotDangling(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include(path/to/missing.cmake OPTIONAL)\n")

	result, err := Walk(context.Background(), root, dir, Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(result.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(result.Edges))
	}
	if got := result.Edges[0].Status.String(); got != "optional_absent" {
		t.Fatalf("OPTIONAL missing include status = %q, want optional_absent", got)
	}
	if result.Histogram.Dangling != 0 {
		t.Fatalf("dangling histogram = %d, want 0 for an allowed OPTIONAL absence", result.Histogram.Dangling)
	}
}

func assertCoverageReason(t *testing.T, result *Result, want string) {
	t.Helper()
	for _, hole := range result.UnscannedFiles {
		if string(hole.Reason) == want {
			return
		}
	}
	t.Fatalf("coverage reasons = %+v, want %q", result.UnscannedFiles, want)
}
