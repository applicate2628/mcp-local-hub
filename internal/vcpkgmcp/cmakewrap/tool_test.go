package cmakewrap

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// TestRunGraph_RealWalkTree wires the ACTUAL cmakegraph.WalkTree against a
// tiny real fixture tree — proving this is a thin wrapper (no re-implemented
// resolution), not a second parallel implementation.
func TestRunGraph_RealWalkTree(t *testing.T) {
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(workdir, "testdata", "cmake_fixture")
	res := RunGraph(context.Background(), Args{Root: fixture})
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
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(workdir, "testdata", "cmake_fixture", "CMakeLists.txt")
	res := RunGraph(context.Background(), Args{File: fixture})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if res.Histogram.Resolved+res.Histogram.Dangling+res.Histogram.Unresolved != 3 {
		t.Errorf("histogram total = %+v, want 3 edges total", res.Histogram)
	}
}

func TestRun_ArgsInvalid_NeitherRootNorFile(t *testing.T) {
	res := run(context.Background(), Args{}, cmakegraph.Walk, cmakegraph.WalkTree)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonArgsInvalid {
		t.Fatalf("got status=%v reason=%v, want unknown/args_invalid", res.Status, res.Reason)
	}
}

func TestRun_WalkFailed_PropagatesAsUnknown(t *testing.T) {
	fakeWalk := func(ctx context.Context, startFile, workspaceRoot string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		return nil, errors.New("simulated walk failure")
	}
	fakeWalkTree := func(ctx context.Context, root, workspaceRoot string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		return nil, errors.New("simulated walktree failure")
	}
	res := run(context.Background(), Args{File: filepath.Join(t.TempDir(), "does-not-matter.cmake")}, fakeWalk, fakeWalkTree)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonWalkFailed {
		t.Fatalf("got status=%v reason=%v, want unknown/walk_failed", res.Status, res.Reason)
	}

	res2 := run(context.Background(), Args{Root: t.TempDir()}, fakeWalk, fakeWalkTree)
	if res2.Status != evidence.StatusUnknown || res2.Reason != ReasonWalkFailed {
		t.Fatalf("got status=%v reason=%v, want unknown/walk_failed (root mode)", res2.Status, res2.Reason)
	}
}

// TestRun_HistogramIsVerbatimNotRederived proves the histogram field is a
// literal pass-through of cmakegraph's own tally, not something this
// wrapper recomputes from the edges list.
func TestRun_HistogramIsVerbatimNotRederived(t *testing.T) {
	wantHist := cmakegraph.Histogram{Resolved: 7, Dangling: 2, Unresolved: 1, ByReason: map[cmakegraph.Reason]int{cmakegraph.ReasonParseError: 1}}
	fakeWalkTree := func(ctx context.Context, root, workspaceRoot string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		return &cmakegraph.Result{
			Root:          root,
			WorkspaceRoot: root,
			Histogram:     wantHist,
		}, nil
	}
	res := run(context.Background(), Args{Root: t.TempDir()}, cmakegraph.Walk, fakeWalkTree)
	if res.Histogram.Resolved != 7 || res.Histogram.Dangling != 2 || res.Histogram.Unresolved != 1 {
		t.Fatalf("histogram = %+v, want verbatim %+v", res.Histogram, wantHist)
	}
	if res.Histogram.ByReason[cmakegraph.ReasonParseError] != 1 {
		t.Fatalf("by_reason not passed through verbatim: %+v", res.Histogram.ByReason)
	}
}

// =====================================================================
// Pre-submission cross-family review, round 2 (F19/F20/F21).
// =====================================================================

// F19: root and file are declared mutually exclusive by the tool schema.
// Honouring root and silently discarding file returned ok for an analysis of
// a tree the caller may not have asked about — a confident answer to a
// different question.
func TestF19_BothRootAndFileIsRefusedNotSilentlyResolvedInRootsFavour(t *testing.T) {
	var walkTreeCalled, walkCalled bool
	fakeWalk := func(ctx context.Context, startFile, workspaceRoot string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		walkCalled = true
		return &cmakegraph.Result{}, nil
	}
	fakeWalkTree := func(ctx context.Context, root, workspaceRoot string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		walkTreeCalled = true
		return &cmakegraph.Result{}, nil
	}

	res := run(context.Background(), Args{Root: "some-tree", File: "some-file.cmake"}, fakeWalk, fakeWalkTree)

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonArgsInvalid {
		t.Fatalf("status=%v reason=%v, want unknown/args_invalid for mutually exclusive root+file", res.Status, res.Reason)
	}
	if walkTreeCalled || walkCalled {
		t.Fatalf("a walk was executed for an invalid argument combination (walkTree=%v walk=%v); "+
			"the refusal must happen before any traversal", walkTreeCalled, walkCalled)
	}
}

// F20: workspace_root must reach cmakegraph in ROOT mode. It used to be
// computed and then dropped, so WalkTree hardwired the workspace to root and
// every include reaching the rest of the caller's workspace was reported as
// escaping it.
func TestF20_WorkspaceRootIsForwardedInRootMode(t *testing.T) {
	var gotRoot, gotWorkspace string
	fakeWalkTree := func(ctx context.Context, root, workspaceRoot string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		gotRoot, gotWorkspace = root, workspaceRoot
		return &cmakegraph.Result{Root: root, WorkspaceRoot: workspaceRoot}, nil
	}

	root := filepath.Join(t.TempDir(), "tree")
	workspace := t.TempDir()
	run(context.Background(), Args{Root: root, WorkspaceRoot: workspace}, cmakegraph.Walk, fakeWalkTree)

	if gotRoot != root {
		t.Errorf("root forwarded as %q, want %q", gotRoot, root)
	}
	if gotWorkspace != workspace {
		t.Fatalf("workspace_root forwarded as %q, want %q — supplying it must not be silently ignored "+
			"in root mode", gotWorkspace, "C:/workspace")
	}
}

// F20 (companion): an omitted workspace_root still defaults to root, so the
// forwarding fix does not change the documented default.
func TestF20_OmittedWorkspaceRootStillDefaultsToRoot(t *testing.T) {
	var gotWorkspace string
	fakeWalkTree := func(ctx context.Context, root, workspaceRoot string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
		gotWorkspace = workspaceRoot
		return &cmakegraph.Result{}, nil
	}

	root := t.TempDir()
	run(context.Background(), Args{Root: root}, cmakegraph.Walk, fakeWalkTree)

	if gotWorkspace != root {
		t.Fatalf("default workspace_root = %q, want the root %q", gotWorkspace, root)
	}
}

// F21: the default workspace for a ROOT-LEVEL file was computed by hand-rolled
// separator arithmetic, producing "C:" (a DRIVE-RELATIVE path whose meaning
// depends on the process's per-drive working directory) or "" (nothing at
// all). filepath.Dir is the single owner of this operation.
func TestF21_RootLevelFileYieldsAValidDefaultWorkspace(t *testing.T) {
	cases := []struct {
		name string
		file string
		// wantAbs is false for the posix-style input ON WINDOWS, where
		// filepath.Dir correctly yields the drive-relative-root `\`:
		// filepath.IsAbs demands a volume there. `\` is still a real rooted
		// path — the defect being fixed was the EMPTY string.
		wantAbs bool
	}{
		{name: "windows drive root", file: `C:\CMakeLists.txt`, wantAbs: runtime.GOOS == "windows"},
		{name: "posix style root", file: "/CMakeLists.txt", wantAbs: runtime.GOOS != "windows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !filepath.IsAbs(tc.file) {
				t.Skipf("%q is not absolute under this host's filepath semantics", tc.file)
			}
			var gotWorkspace string
			fakeWalk := func(ctx context.Context, startFile, workspaceRoot string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
				gotWorkspace = workspaceRoot
				return &cmakegraph.Result{}, nil
			}

			run(context.Background(), Args{File: tc.file}, fakeWalk, cmakegraph.WalkTree)

			if gotWorkspace == "" {
				t.Fatalf("default workspace for %q is EMPTY — resolution would fall back to process state", tc.file)
			}
			if gotWorkspace == "C:" {
				t.Fatalf("default workspace for %q is the drive-relative %q, whose meaning depends on the "+
					"process's per-drive working directory", tc.file, gotWorkspace)
			}
			if tc.wantAbs && !filepath.IsAbs(gotWorkspace) {
				t.Fatalf("default workspace for %q = %q, want an absolute path", tc.file, gotWorkspace)
			}
			if want := filepath.Dir(tc.file); gotWorkspace != want {
				t.Fatalf("default workspace for %q = %q, want filepath.Dir = %q", tc.file, gotWorkspace, want)
			}
		})
	}
}

// A canceled context is reported as unknown(canceled), distinct from
// walk_failed: one says the tree is broken, the other says we stopped looking.
func TestRun_CanceledContextIsItsOwnReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := run(ctx, Args{Root: t.TempDir()}, cmakegraph.Walk, cmakegraph.WalkTree)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonCanceled {
		t.Fatalf("status=%v reason=%v, want unknown/canceled", res.Status, res.Reason)
	}
}

// TestPR591_RelativeGraphPathsRejectedBeforeTraversal proves the wrapper
// refuses any non-empty relative graph path before it can bind to the hub
// daemon working directory or invoke a traversal callback.
func TestPR591_RelativeGraphPathsRejectedBeforeTraversal(t *testing.T) {
	absoluteRoot := t.TempDir()
	absoluteWorkspace := t.TempDir()
	absoluteFile := filepath.Join(t.TempDir(), "CMakeLists.txt")

	cases := []struct {
		name          string
		args          Args
		wantWalk      int
		wantWalkTree  int
		wantArgsError bool
		relativePath  string
	}{
		{name: "relative root", args: Args{Root: "relative-root"}, wantArgsError: true, relativePath: "relative-root"},
		{name: "drive-relative root", args: Args{Root: `C:relative-root`}, wantArgsError: true, relativePath: `C:relative-root`},
		{name: "relative file", args: Args{File: "relative-file.cmake"}, wantArgsError: true, relativePath: "relative-file.cmake"},
		{name: "explicit relative workspace root", args: Args{Root: absoluteRoot, WorkspaceRoot: "relative-workspace"}, wantArgsError: true, relativePath: "relative-workspace"},
		{name: "absolute root control", args: Args{Root: absoluteRoot, WorkspaceRoot: absoluteWorkspace}, wantWalkTree: 1},
		{name: "absolute file control", args: Args{File: absoluteFile, WorkspaceRoot: absoluteWorkspace}, wantWalk: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantArgsError {
				if filepath.IsAbs(tc.relativePath) {
					t.Fatalf("rejection input %q is absolute under filepath semantics", tc.relativePath)
				}
			}

			var walkCalls, walkTreeCalls int
			fakeWalk := func(ctx context.Context, startFile, workspaceRoot string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
				walkCalls++
				return &cmakegraph.Result{Root: startFile, WorkspaceRoot: workspaceRoot}, nil
			}
			fakeWalkTree := func(ctx context.Context, root, workspaceRoot string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
				walkTreeCalls++
				return &cmakegraph.Result{Root: root, WorkspaceRoot: workspaceRoot}, nil
			}

			res := run(context.Background(), tc.args, fakeWalk, fakeWalkTree)
			if tc.wantArgsError {
				if res.Status != evidence.StatusUnknown || res.Reason != ReasonArgsInvalid {
					t.Fatalf("status=%v reason=%v, want unknown/args_invalid", res.Status, res.Reason)
				}
			} else if res.Status != evidence.StatusOK {
				t.Fatalf("status=%v reason=%v, want ok", res.Status, res.Reason)
			}
			if walkCalls != tc.wantWalk || walkTreeCalls != tc.wantWalkTree {
				t.Fatalf("walk calls=%d walkTree calls=%d, want %d and %d", walkCalls, walkTreeCalls, tc.wantWalk, tc.wantWalkTree)
			}
		})
	}
}

func TestPR591_CMakeWrapPublicDocsMatchAbsolutePathContract(t *testing.T) {
	for _, field := range []string{"Root", "File", "WorkspaceRoot"} {
		doc := argsFieldDoc(t, field)
		if !strings.Contains(strings.ToLower(doc), "absolute") || !strings.Contains(doc, "unknown(args_invalid)") {
			t.Fatalf("Args.%s doc = %q, want absolute-path and unknown(args_invalid) contract", field, doc)
		}
	}
}

func argsFieldDoc(t *testing.T, fieldName string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), thisFile[:len(thisFile)-len("_test.go")]+".go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse tool.go: %v", err)
	}
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "Args" {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatal("Args is not a struct")
			}
			for _, field := range structType.Fields.List {
				if len(field.Names) == 1 && field.Names[0].Name == fieldName {
					if field.Doc == nil {
						return ""
					}
					return field.Doc.Text()
				}
			}
		}
	}
	t.Fatalf("Args.%s not found", fieldName)
	return ""
}

// End-to-end over the REAL cmakegraph: the wrapper forwards the coverage
// signals rather than presenting a bounded scan as a complete one.
func TestRunGraph_ForwardsRootEnumerationCapVerbatim(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.cmake", "b.cmake", "c.cmake"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("# leaf\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := RunGraph(context.Background(), Args{Root: root, MaxRoots: 1})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status=%v reason=%v, want ok", res.Status, res.Reason)
	}
	if !res.RootEnumerationCapped {
		t.Fatal("root_enumeration_capped = false, want true — the wrapper must forward the cap signal")
	}
	var sawCap bool
	for _, u := range res.UnscannedFiles {
		if u.Reason == cmakegraph.CoverageRootEnumerationCapped {
			sawCap = true
		}
	}
	if !sawCap {
		t.Fatalf("unscanned_files = %+v, want the root_enumeration_capped coverage hole forwarded verbatim", res.UnscannedFiles)
	}
}

// F20 CONFIRMED IN THE FIELD. The operator's call was:
//
//	{"root": ".../overlays/triplets", "workspace_root": "c:/vcpkg/vcpkg-builds"}
//
// and the response ECHOED "workspace_root": ".../overlays/triplets" — the
// supplied boundary silently replaced by root. 8 edges came back
// unresolved/outside_workspace that resolve correctly inside the named
// workspace. The ECHO is where this was caught, so the echo is what this
// asserts.
func TestF20_Field_EchoedWorkspaceRootEqualsTheSuppliedOne(t *testing.T) {
	workspace := t.TempDir()
	toolchains := filepath.Join(workspace, "toolchains", "common")
	triplets := filepath.Join(workspace, "overlays", "triplets")
	for _, d := range []string{toolchains, triplets} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(toolchains, "compiler-command-logging.cmake"), []byte("# leaf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The operator's real shape: a triplet file reaching UP out of overlays/
	// into a sibling directory of the same workspace.
	if err := os.WriteFile(filepath.Join(triplets, "x64-windows.cmake"),
		[]byte("include(${CMAKE_CURRENT_LIST_DIR}/../../toolchains/common/compiler-command-logging.cmake)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := RunGraph(context.Background(), Args{Root: triplets, WorkspaceRoot: workspace})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status=%v reason=%v, want ok", res.Status, res.Reason)
	}

	// The echo is the contract surface the operator read.
	wantEcho, err := filepath.Abs(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkspaceRoot != wantEcho {
		t.Fatalf("echoed workspace_root = %q, want the SUPPLIED %q — silently substituting root is what "+
			"turned 8 correct edges into outside_workspace findings", res.WorkspaceRoot, wantEcho)
	}
	if res.Histogram.Resolved != 1 || res.Histogram.Unresolved != 0 {
		t.Fatalf("histogram = %+v, want the ../.. include RESOLVED inside the supplied workspace, not "+
			"reported as escaping it; edges=%+v", res.Histogram, res.Edges)
	}
}

// F20 (the operator's own stated preference): if the supplied boundary CANNOT
// be honoured — root is not inside workspace_root — fail LOUDLY rather than
// silently narrowing it. Silent narrowing is what turns correct edges into
// findings.
func TestF20_Field_RootOutsideSuppliedWorkspaceFailsLoudlyNotSilently(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	elsewhere := filepath.Join(base, "elsewhere")
	for _, d := range []string{workspace, elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "a.cmake"), []byte("# leaf\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := RunGraph(context.Background(), Args{Root: elsewhere, WorkspaceRoot: workspace})

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonWalkFailed {
		t.Fatalf("status=%v reason=%v, want unknown/walk_failed — a root outside the supplied workspace must "+
			"be refused, never quietly resolved by substituting root for the boundary", res.Status, res.Reason)
	}
	if len(res.Edges) != 0 {
		t.Fatalf("edges = %+v, want none: the call was refused", res.Edges)
	}
}
