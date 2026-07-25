package cmakewrap

import (
	"errors"
	"testing"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
	"mcp-local-hub/internal/cmakegraph"
)

// TestRunGraph_RealWalkTree wires the ACTUAL cmakegraph.WalkTree against a
// tiny real fixture tree — proving this is a thin wrapper (no re-implemented
// resolution), not a second parallel implementation.
func TestRunGraph_RealWalkTree(t *testing.T) {
	res := RunGraph(Args{Root: "testdata/cmake_fixture"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if res.Histogram.Resolved != 2 {
		t.Errorf("histogram.resolved = %d, want 2 (helper.cmake + sub/CMakeLists.txt)", res.Histogram.Resolved)
	}
	if res.Histogram.Dangling != 1 {
		t.Errorf("histogram.dangling = %d, want 1 (missing_helper.cmake)", res.Histogram.Dangling)
	}
	if res.Histogram.Unresolved != 0 {
		t.Errorf("histogram.unresolved = %d, want 0", res.Histogram.Unresolved)
	}
	if len(res.Edges) != 3 {
		t.Fatalf("got %d edges, want 3: %+v", len(res.Edges), res.Edges)
	}
	var sawDangling bool
	for _, e := range res.Edges {
		if e.Status == "dangling" {
			sawDangling = true
			if e.RawArg != "missing_helper.cmake" {
				t.Errorf("dangling edge raw_arg = %q", e.RawArg)
			}
		}
		// Status must be the verbatim cmakegraph string form, never a
		// re-derived label.
		if e.Status != "resolved" && e.Status != "dangling" && e.Status != "unresolved" {
			t.Errorf("unexpected status string %q — must be cmakegraph's own String() output", e.Status)
		}
	}
	if !sawDangling {
		t.Error("expected to see the dangling missing_helper.cmake edge")
	}
}

// TestRunGraph_RealWalk_SingleFile exercises the Walk (single-file) mode.
func TestRunGraph_RealWalk_SingleFile(t *testing.T) {
	res := RunGraph(Args{File: "testdata/cmake_fixture/CMakeLists.txt"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if res.Histogram.Resolved+res.Histogram.Dangling+res.Histogram.Unresolved != 3 {
		t.Errorf("histogram total = %+v, want 3 edges total", res.Histogram)
	}
}

func TestRun_ArgsInvalid_NeitherRootNorFile(t *testing.T) {
	res := run(Args{}, cmakegraph.Walk, cmakegraph.WalkTree)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonArgsInvalid {
		t.Fatalf("got status=%v reason=%v, want unknown/args_invalid", res.Status, res.Reason)
	}
}

func TestRun_WalkFailed_PropagatesAsUnknown(t *testing.T) {
	fakeWalk := func(startFile, workspaceRoot string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		return nil, errors.New("simulated walk failure")
	}
	fakeWalkTree := func(root string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		return nil, errors.New("simulated walktree failure")
	}
	res := run(Args{File: "does-not-matter.cmake"}, fakeWalk, fakeWalkTree)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonWalkFailed {
		t.Fatalf("got status=%v reason=%v, want unknown/walk_failed", res.Status, res.Reason)
	}

	res2 := run(Args{Root: "does-not-matter"}, fakeWalk, fakeWalkTree)
	if res2.Status != evidence.StatusUnknown || res2.Reason != ReasonWalkFailed {
		t.Fatalf("got status=%v reason=%v, want unknown/walk_failed (root mode)", res2.Status, res2.Reason)
	}
}

// TestRun_HistogramIsVerbatimNotRederived proves the histogram field is a
// literal pass-through of cmakegraph's own tally, not something this
// wrapper recomputes from the edges list.
func TestRun_HistogramIsVerbatimNotRederived(t *testing.T) {
	wantHist := cmakegraph.Histogram{Resolved: 7, Dangling: 2, Unresolved: 1, ByReason: map[cmakegraph.Reason]int{cmakegraph.ReasonParseError: 1}}
	fakeWalkTree := func(root string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		return &cmakegraph.Result{
			Root:          root,
			WorkspaceRoot: root,
			Histogram:     wantHist,
		}, nil
	}
	res := run(Args{Root: "irrelevant"}, cmakegraph.Walk, fakeWalkTree)
	if res.Histogram.Resolved != 7 || res.Histogram.Dangling != 2 || res.Histogram.Unresolved != 1 {
		t.Fatalf("histogram = %+v, want verbatim %+v", res.Histogram, wantHist)
	}
	if res.Histogram.ByReason[cmakegraph.ReasonParseError] != 1 {
		t.Fatalf("by_reason not passed through verbatim: %+v", res.Histogram.ByReason)
	}
}
