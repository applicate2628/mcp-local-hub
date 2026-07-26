package cmakegraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return full
}

// realpath canonicalizes p exactly the way this package's internal
// canonicalize() does (Abs + EvalSymlinks-if-possible). Tests must build
// "want" paths through this helper whenever comparing against a
// ${CMAKE_CURRENT_LIST_DIR}-derived or bare-add_subdirectory-derived
// ResolvedPath/FromFile, because those flow through the package's
// symlink-resolved canonical form — which on some hosts (observed: this
// environment's mapped temp drive) differs in CASE from the raw path
// t.TempDir() returns, even though no symlink is actually involved. This is
// a real, probed filesystem property of the host, not a package defect:
// ${CMAKE_SOURCE_DIR} substitutions deliberately use the RAW (non-canonical)
// workspace root instead (see package doc), so tests comparing against THAT
// substitution must NOT use this helper.
func realpath(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("Abs(%q): %v", p, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func edgeByLine(t *testing.T, res *Result, line int) Edge {
	t.Helper()
	for _, e := range res.Edges {
		if e.Line == line {
			return e
		}
	}
	t.Fatalf("no edge at line %d; edges=%+v", line, res.Edges)
	return Edge{}
}

// --- Fixture 1: ${CMAKE_CURRENT_LIST_DIR} chain -----------------------------
//
// Proves CMAKE_CURRENT_LIST_DIR is resolved per-FILE, not pinned to the root:
// root includes sub/helper.cmake via ${CMAKE_CURRENT_LIST_DIR}, and
// helper.cmake ITSELF includes deeper.cmake the same way — the second
// substitution must use helper.cmake's OWN directory (sub/), not root's.
func TestCurrentListDirChain_ResolvesPerFileNotPinnedToRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/sub/helper.cmake)`)
	writeFile(t, root, "sub/helper.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/deeper.cmake)`)
	writeFile(t, root, "sub/deeper.cmake", `# leaf file, no further includes`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 2 {
		t.Fatalf("edges = %d, want 2: %+v", len(res.Edges), res.Edges)
	}
	rootEdge := edgeByLine(t, res, 1)
	if rootEdge.Status != StatusResolved {
		t.Fatalf("root include status = %v, want resolved", rootEdge.Status)
	}
	// ${CMAKE_CURRENT_LIST_DIR} substitutions flow through this package's
	// canonicalized (symlink-resolved) directory form — see realpath's doc.
	canonRoot := realpath(t, root)
	wantHelper := filepath.Join(canonRoot, "sub", "helper.cmake")
	if rootEdge.ResolvedPath != wantHelper {
		t.Fatalf("root include resolved = %q, want %q", rootEdge.ResolvedPath, wantHelper)
	}

	var helperEdge *Edge
	for i := range res.Edges {
		if res.Edges[i].FromFile == wantHelper {
			helperEdge = &res.Edges[i]
		}
	}
	if helperEdge == nil {
		t.Fatalf("no edge discovered FROM helper.cmake; edges=%+v", res.Edges)
	}
	if helperEdge.Status != StatusResolved {
		t.Fatalf("helper include status = %v, want resolved", helperEdge.Status)
	}
	wantDeeper := filepath.Join(canonRoot, "sub", "deeper.cmake")
	if helperEdge.ResolvedPath != wantDeeper {
		t.Fatalf("helper's ${CMAKE_CURRENT_LIST_DIR} substitution = %q, want %q (must use helper's OWN dir, not root's)",
			helperEdge.ResolvedPath, wantDeeper)
	}
	if res.Histogram.Resolved != 2 || res.Histogram.Dangling != 0 || res.Histogram.Unresolved != 0 {
		t.Fatalf("histogram = %+v, want {Resolved:2}", res.Histogram)
	}
}

// --- Fixture 2 (+ mutation proof): dangling include --------------------------
//
// This is the named mutation-proof test: a resolver that forgot to check
// file existence (e.g. always reporting Resolved once a path is computed)
// fails BOTH assertions below independently.
func TestMutationProof_DanglingPathNeverReportedResolved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/does-not-exist.cmake)`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	wantPath := filepath.Join(realpath(t, root), "does-not-exist.cmake")
	if _, statErr := os.Stat(wantPath); !os.IsNotExist(statErr) {
		t.Fatalf("test fixture invariant broken: %q unexpectedly exists", wantPath)
	}
	// Assertion A: Status must be Dangling, never Resolved, for a path that
	// does not exist on disk.
	if e.Status != StatusDangling {
		t.Fatalf("status = %v, want StatusDangling — a mutation that reports Resolved regardless of os.Stat must fail this line", e.Status)
	}
	// Assertion B: the histogram must agree (independent code path from A —
	// the histogram is computed by summing e.Status in Result(), so a
	// mutation that fixes one but not the other still gets caught).
	if res.Histogram.Resolved != 0 || res.Histogram.Dangling != 1 {
		t.Fatalf("histogram = %+v, want {Dangling:1, Resolved:0}", res.Histogram)
	}
	if e.ResolvedPath != wantPath {
		t.Fatalf("dangling edge must still record the computed path so an operator can see WHAT was missing: got %q, want %q", e.ResolvedPath, wantPath)
	}
}

// --- Fixture 3: cycle --------------------------------------------------------
//
// a.cmake includes b.cmake, b.cmake includes a.cmake back — a genuine
// circular reference. Must terminate (not stack-overflow / infinite loop)
// and classify the closing edge as cyclic, never resolved.
func TestCycle_DetectedNotInfiniteLoop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/a.cmake)`)
	writeFile(t, root, "a.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/b.cmake)`)
	writeFile(t, root, "b.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/a.cmake)`)

	done := make(chan *Result, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
		if err != nil {
			errCh <- err
			return
		}
		done <- res
	}()
	select {
	case err := <-errCh:
		t.Fatalf("Walk: %v", err)
	case res := <-done:
		// Exactly 3 edges: root->a (resolved), a->b (resolved), b->a (cyclic).
		if len(res.Edges) != 3 {
			t.Fatalf("edges = %d, want 3 (a genuine cycle must not keep expanding): %+v", len(res.Edges), res.Edges)
		}
		canonRoot := realpath(t, root)
		aFile := filepath.Join(canonRoot, "a.cmake")
		var closingEdge *Edge
		for i := range res.Edges {
			if res.Edges[i].FromFile == filepath.Join(canonRoot, "b.cmake") {
				closingEdge = &res.Edges[i]
			}
		}
		if closingEdge == nil {
			t.Fatalf("no edge found from b.cmake; edges=%+v", res.Edges)
		}
		if closingEdge.Status != StatusUnresolved || closingEdge.Reason != ReasonCyclic {
			t.Fatalf("closing edge = {%v,%v}, want {Unresolved,cyclic}", closingEdge.Status, closingEdge.Reason)
		}
		if closingEdge.ResolvedPath != aFile {
			t.Fatalf("cyclic edge ResolvedPath = %q, want %q", closingEdge.ResolvedPath, aFile)
		}
		if res.Histogram.ByReason[ReasonCyclic] != 1 {
			t.Fatalf("histogram cyclic count = %d, want 1", res.Histogram.ByReason[ReasonCyclic])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Walk did not return — likely an infinite loop on a genuine include cycle")
	}
}

// --- Fixture 4a: plain ".." escape (always exercisable, no OS privilege) ----
func TestOutsideWorkspace_DotDotEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, parent, "outside.cmake", `# lives outside root`)
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/../outside.cmake)`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	// outside.cmake DOES exist on disk — proving the refusal is about the
	// boundary, not a missing file (which would be Dangling instead).
	if e.Status != StatusUnresolved || e.Reason != ReasonOutsideWorkspace {
		t.Fatalf("status = {%v,%v}, want {Unresolved,outside_workspace} (the target exists but escapes root; must not be Resolved)", e.Status, e.Reason)
	}
}

// --- Fixture 4b: symlink escape (skips gracefully if the OS/privilege
// refuses symlink creation, e.g. non-elevated Windows without Developer Mode) -
func TestOutsideWorkspace_SymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, parent, "secret.cmake", `# lives outside root`)
	linkPath := filepath.Join(root, "escape_link")
	if err := os.Symlink(filepath.Join(parent, "secret.cmake"), linkPath); err != nil {
		t.Skipf("symlink creation refused on this host (expected on non-elevated Windows without Developer Mode): %v", err)
	}
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/escape_link)`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	if e.Status != StatusUnresolved || e.Reason != ReasonOutsideWorkspace {
		t.Fatalf("status = {%v,%v}, want {Unresolved,outside_workspace} (symlink resolves outside root; must not be Resolved even though the on-disk symlink entry sits inside root)", e.Status, e.Reason)
	}
}

// --- Fixture 5: non-static variable (including the deliberate
// ${CMAKE_CURRENT_SOURCE_DIR} refusal) ---------------------------------------
func TestNonStaticVariable_UnknownAndCurrentSourceDirBothRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"include(${SOME_OTHER_VAR}/foo.cmake)\n"+
			"include(${CMAKE_CURRENT_SOURCE_DIR}/bar.cmake)\n")

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 2 {
		t.Fatalf("edges = %d, want 2: %+v", len(res.Edges), res.Edges)
	}
	e1 := edgeByLine(t, res, 1)
	if e1.Status != StatusUnresolved || e1.Reason != ReasonNonStaticVariable {
		t.Fatalf("unknown-variable edge = {%v,%v}, want {Unresolved,non_static_variable}", e1.Status, e1.Reason)
	}
	e2 := edgeByLine(t, res, 2)
	if e2.Status != StatusUnresolved || e2.Reason != ReasonNonStaticVariable {
		t.Fatalf("CMAKE_CURRENT_SOURCE_DIR edge = {%v,%v}, want {Unresolved,non_static_variable} — this substitution must be DELIBERATELY refused even though it looks resolvable", e2.Status, e2.Reason)
	}
	if e2.ResolvedPath != "" {
		t.Fatalf("CMAKE_CURRENT_SOURCE_DIR edge must have no ResolvedPath (never computed): got %q", e2.ResolvedPath)
	}
}

// --- Additional coverage: CMAKE_SOURCE_DIR (global constant) resolves ------
func TestCmakeSourceDir_ResolvesToWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_SOURCE_DIR}/tools/helper.cmake)`)
	writeFile(t, root, "tools/helper.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := edgeByLine(t, res, 1)
	want := filepath.Join(root, "tools", "helper.cmake")
	if e.Status != StatusResolved || e.ResolvedPath != want {
		t.Fatalf("edge = {%v,%q}, want {Resolved,%q}", e.Status, e.ResolvedPath, want)
	}
}

// --- Additional coverage: generator expression ------------------------------
func TestGeneratorExpression_NeverEvaluated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include($<BOOL:foo>/gen.cmake)`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := edgeByLine(t, res, 1)
	if e.Status != StatusUnresolved || e.Reason != ReasonGeneratorExpression {
		t.Fatalf("edge = {%v,%v}, want {Unresolved,generator_expression}", e.Status, e.Reason)
	}
}

// --- Additional coverage: bare module name (include-only) -------------------
func TestModuleNameNotPath_BareIncludeName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(GNUInstallDirs)`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := edgeByLine(t, res, 1)
	if e.Status != StatusUnresolved || e.Reason != ReasonModuleNameNotPath {
		t.Fatalf("edge = {%v,%v}, want {Unresolved,module_name_not_path}", e.Status, e.Reason)
	}
}

// --- Additional coverage (D2): Conditional is informational, never gates
// resolution — a conditional call's PATH is resolved by the exact same
// os.Stat rule as an unconditional one, and a Resolved conditional edge is
// still traversed further. -------------------------------------------------
func TestConditional_PathResolvedNormallyOrthogonalToStatus(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"if(SOME_FLAG)\n"+ // line 1
			"  include(${CMAKE_CURRENT_LIST_DIR}/conditional.cmake)\n"+ // line 2: conditional, exists -> Resolved
			"  include(${CMAKE_CURRENT_LIST_DIR}/conditional_missing.cmake)\n"+ // line 3: conditional, absent -> Dangling
			"endif()\n"+ // line 4
			"include(${CMAKE_CURRENT_LIST_DIR}/unconditional.cmake)\n") // line 5: unconditional -> Resolved
	writeFile(t, root, "conditional.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/nested_from_conditional.cmake)`)
	writeFile(t, root, "nested_from_conditional.cmake", `# leaf, reached only by descending into a Resolved conditional edge`)
	writeFile(t, root, "unconditional.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// 3 top-level edges (lines 2, 3, 5) + 1 edge discovered by descending
	// into the Resolved conditional edge at line 2 (proving a conditional
	// Resolved edge is traversed exactly like an unconditional one).
	if len(res.Edges) != 4 {
		t.Fatalf("edges = %d, want 4: %+v", len(res.Edges), res.Edges)
	}

	resolvedCond := edgeByLine(t, res, 2)
	if resolvedCond.Status != StatusResolved || !resolvedCond.Conditional {
		t.Fatalf("conditional+existing edge = {%v,Conditional:%v}, want {Resolved,Conditional:true} — the path must be resolved despite the if() guard", resolvedCond.Status, resolvedCond.Conditional)
	}

	danglingCond := edgeByLine(t, res, 3)
	if danglingCond.Status != StatusDangling || !danglingCond.Conditional {
		t.Fatalf("conditional+missing edge = {%v,Conditional:%v}, want {Dangling,Conditional:true} — Conditional must not force Resolved, and Dangling must not force Conditional:false", danglingCond.Status, danglingCond.Conditional)
	}

	uncondEdge := edgeByLine(t, res, 5)
	if uncondEdge.Status != StatusResolved || uncondEdge.Conditional {
		t.Fatalf("unconditional edge = {%v,Conditional:%v}, want {Resolved,Conditional:false} — must not be poisoned by an unrelated if-block", uncondEdge.Status, uncondEdge.Conditional)
	}

	// The edge discovered inside conditional.cmake (reached by descending
	// past the Resolved conditional edge) is itself NOT conditional — its
	// own file has no if() around it. Conditional is per-file-lexical, not
	// transitive (see package doc).
	var nestedEdge *Edge
	for i := range res.Edges {
		if filepath.Base(res.Edges[i].FromFile) == "conditional.cmake" {
			nestedEdge = &res.Edges[i]
		}
	}
	if nestedEdge == nil {
		t.Fatalf("expected an edge discovered from conditional.cmake (proves recursion past a conditional Resolved edge); edges=%+v", res.Edges)
	}
	if nestedEdge.Status != StatusResolved || nestedEdge.Conditional {
		t.Fatalf("nested edge from conditional.cmake = {%v,Conditional:%v}, want {Resolved,Conditional:false} (Conditional is not transitive)", nestedEdge.Status, nestedEdge.Conditional)
	}

	// ReasonConditionalBranch no longer exists in the Reason enum at all (D2
	// removed it) — every edge above landed on Resolved/Dangling, and the
	// histogram's Unresolved bucket must be empty since none of this
	// fixture's edges hit any OTHER unresolved reason either.
	if res.Histogram.Unresolved != 0 {
		t.Fatalf("Unresolved count = %d, want 0: %+v", res.Histogram.Unresolved, res.Edges)
	}
}

// --- Additional coverage: add_subdirectory, bare relative name --------------
func TestAddSubdirectory_BareRelativeName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `add_subdirectory(child)`)
	writeFile(t, root, "child/CMakeLists.txt", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := edgeByLine(t, res, 1)
	want := filepath.Join(realpath(t, root), "child")
	if e.Status != StatusResolved || e.Kind != EdgeAddSubdirectory || e.ResolvedPath != want {
		t.Fatalf("edge = {%v,%v,%q}, want {Resolved,add_subdirectory,%q}", e.Status, e.Kind, e.ResolvedPath, want)
	}
	if len(res.Files) != 2 {
		t.Fatalf("scanned files = %v, want 2 (root + child/CMakeLists.txt)", res.Files)
	}
}

// add_subdirectory pointing at a directory that has no CMakeLists.txt inside
// is Dangling, not Resolved — CMake itself would fail to configure here.
func TestAddSubdirectory_MissingCMakeListsIsDangling(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `add_subdirectory(child)`)
	if err := os.MkdirAll(filepath.Join(root, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	e := edgeByLine(t, res, 1)
	if e.Status != StatusDangling {
		t.Fatalf("status = %v, want Dangling (directory exists but has no CMakeLists.txt)", e.Status)
	}
}

// --- Additional coverage: depth limit ---------------------------------------
func TestDepthLimit_StopsExpansionAndReportsReason(t *testing.T) {
	root := t.TempDir()
	// A chain of 5 includes: root -> f1 -> f2 -> f3 -> f4 (leaf).
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/f1.cmake)`)
	writeFile(t, root, "f1.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/f2.cmake)`)
	writeFile(t, root, "f2.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/f3.cmake)`)
	writeFile(t, root, "f3.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/f4.cmake)`)
	writeFile(t, root, "f4.cmake", `# leaf`)

	// MaxDepth=2 means: root(depth0) -> f1(depth1) -> f2(depth2) resolve, but
	// the edge f2->f3 (which would be depth3) must be refused as depth_limit.
	opts := Options{MaxDepth: 2, MaxNodes: DefaultMaxNodes, MaxFileBytes: DefaultMaxFileBytes}
	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Histogram.ByReason[ReasonDepthLimit] != 1 {
		t.Fatalf("depth_limit count = %d, want 1: edges=%+v", res.Histogram.ByReason[ReasonDepthLimit], res.Edges)
	}
	if res.Histogram.Resolved != 2 {
		t.Fatalf("resolved count = %d, want 2 (root->f1, f1->f2)", res.Histogram.Resolved)
	}
	// f3.cmake and f4.cmake must never have been opened.
	for _, f := range res.Files {
		if filepath.Base(f) == "f3.cmake" || filepath.Base(f) == "f4.cmake" {
			t.Fatalf("depth_limit must stop traversal before scanning %s", f)
		}
	}
}

// --- Additional coverage: total node cap ------------------------------------
func TestNodeCap_StopsExpansionAndReportsReason(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"include(${CMAKE_CURRENT_LIST_DIR}/a.cmake)\n"+
			"include(${CMAKE_CURRENT_LIST_DIR}/b.cmake)\n")
	writeFile(t, root, "a.cmake", `# leaf a`)
	writeFile(t, root, "b.cmake", `# leaf b`)

	// MaxNodes=1: only the root itself counts as visited from the start, so
	// the FIRST new file (a.cmake) is allowed (visited size 1 < cap... need
	// cap=1 to refuse BOTH new children). Use MaxNodes=1 so even the first
	// child is refused (root alone already occupies the single slot).
	opts := Options{MaxDepth: DefaultMaxDepth, MaxNodes: 1, MaxFileBytes: DefaultMaxFileBytes}
	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Histogram.Resolved != 0 {
		t.Fatalf("resolved count = %d, want 0 (root already fills the 1-node cap)", res.Histogram.Resolved)
	}
	if res.Histogram.ByReason[ReasonDepthLimit] != 2 {
		t.Fatalf("depth_limit count = %d, want 2 (both a.cmake and b.cmake refused)", res.Histogram.ByReason[ReasonDepthLimit])
	}
}

// --- Additional coverage: parse error (unterminated call) -------------------
func TestParseError_UnterminatedCall(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/unterminated.cmake`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	if res.Edges[0].Status != StatusUnresolved || res.Edges[0].Reason != ReasonParseError {
		t.Fatalf("edge = {%v,%v}, want {Unresolved,parse_error}", res.Edges[0].Status, res.Edges[0].Reason)
	}
}

// --- Additional coverage: comments never produce edges ----------------------
func TestCommentedOutInclude_NotDiscovered(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "# include(${CMAKE_CURRENT_LIST_DIR}/never.cmake)\n")

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 0 {
		t.Fatalf("edges = %d, want 0 (commented-out call must not be scanned): %+v", len(res.Edges), res.Edges)
	}
}

// --- Additional coverage (D3): a command name embedded inside an unrelated
// string-literal argument (the vcpkg_replace_string(...) pattern actually
// observed against the operator's real tree) produces NO edge — it is
// skipped and counted, never treated as a real directive. --------------------
func TestStringLiteralGuard_CommandNameInsideStringIsSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"vcpkg_replace_string(\n"+
			`    "${SOURCE_PATH}/CMakeLists.txt"`+"\n"+
			`    "add_subdirectory(src/Tutorials/"`+"\n"+
			`    "# add_subdirectory(src/Tutorials/"`+"\n"+
			")\n"+
			"include(${CMAKE_CURRENT_LIST_DIR}/real.cmake)\n")
	writeFile(t, root, "real.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// Only the genuine include() survives as an edge; the two
	// add_subdirectory(-shaped occurrences inside the quoted find/replace
	// strings must produce NO edge at all.
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1 (only the real include(), none from the quoted find/replace text): %+v", len(res.Edges), res.Edges)
	}
	if res.Edges[0].Kind != EdgeInclude || res.Edges[0].Status != StatusResolved {
		t.Fatalf("surviving edge = %+v, want the real Resolved include()", res.Edges[0])
	}
	// The single-pass lexer treats the WHOLE vcpkg_replace_string(...) call
	// as one opaque top-level command span (its quoted arguments, including
	// the add_subdirectory(-shaped text, are consumed by skipQuoted and never
	// independently re-scanned for embedded commands) — there is no separate
	// "skipped" counter to check anymore; the guarantee is structural, and
	// len(res.Edges) == 1 above already proves it held.
}

// --- Additional coverage (D1): WalkTree with a "*.cmake" pattern reaches
// files no portfile.cmake ever references directly — e.g. vcpkg's own
// toolchain/port-tweak helper files — which a portfile.cmake-only root scan
// would have missed entirely. -----------------------------------------------
func TestWalkTree_WildcardPatternReachesFilesNoPortfileReferences(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ports/someport/portfile.cmake", `# does not include the toolchain at all`)
	writeFile(t, root, "toolchains/mytoolchain.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/toolchain_helper.cmake)`)
	writeFile(t, root, "toolchains/toolchain_helper.cmake", `# leaf`)

	narrow, err := WalkTree(context.Background(), root, root, []string{"portfile.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree (narrow): %v", err)
	}
	if len(narrow.Edges) != 0 {
		t.Fatalf("narrow (portfile.cmake-only) scan edges = %d, want 0 — the toolchain file is unreachable from any portfile.cmake: %+v", len(narrow.Edges), narrow.Edges)
	}

	wide, err := WalkTree(context.Background(), root, root, []string{"*.cmake", "CMakeLists.txt"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree (wide): %v", err)
	}
	if len(wide.Edges) != 1 {
		t.Fatalf("wide (*.cmake) scan edges = %d, want 1 (the toolchain file's own include, discovered by walking it as an independent root): %+v", len(wide.Edges), wide.Edges)
	}
	if wide.Edges[0].Status != StatusResolved {
		t.Fatalf("toolchain edge = %v, want Resolved", wide.Edges[0].Status)
	}
}

// --- Additional coverage (D1): a file reachable BOTH as its own independent
// WalkTree root AND via traversal from a sibling root must be scanned
// exactly once — its edges must never be duplicated in the merged Result. --
func TestWalkTree_SharedFileScannedOnceNotDuplicated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ports/portA/portfile.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/../../common/helper.cmake)`)
	writeFile(t, root, "ports/portB/portfile.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/../../common/helper.cmake)`)
	writeFile(t, root, "common/helper.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/helper_leaf.cmake)`)
	writeFile(t, root, "common/helper_leaf.cmake", `# leaf`)

	res, err := WalkTree(context.Background(), root, root, []string{"*.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	// 3 roots discovered (portA/portfile.cmake, portB/portfile.cmake,
	// common/helper.cmake) produce: portA's include, portB's include, and
	// EXACTLY ONE edge from helper.cmake's own include(helper_leaf.cmake) —
	// never two, even though helper.cmake is both an independent root AND
	// reached via traversal from two different ports.
	helperOwnEdges := 0
	for _, e := range res.Edges {
		if filepath.Base(e.FromFile) == "helper.cmake" {
			helperOwnEdges++
		}
	}
	if helperOwnEdges != 1 {
		t.Fatalf("edges FROM helper.cmake = %d, want exactly 1 (must not be duplicated across the 3 roots that all reach it): %+v", helperOwnEdges, res.Edges)
	}
	if len(res.Edges) != 3 {
		t.Fatalf("total edges = %d, want 3 (portA->helper, portB->helper, helper->leaf, each exactly once): %+v", len(res.Edges), res.Edges)
	}
}

// --- Additional coverage: diamond include is resolved once, not a cycle ----
func TestDiamondInclude_NotMisreportedAsCyclic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"include(${CMAKE_CURRENT_LIST_DIR}/a.cmake)\n"+
			"include(${CMAKE_CURRENT_LIST_DIR}/b.cmake)\n")
	writeFile(t, root, "a.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/shared.cmake)`)
	writeFile(t, root, "b.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/shared.cmake)`)
	writeFile(t, root, "shared.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, e := range res.Edges {
		if filepath.Base(e.ResolvedPath) == "shared.cmake" && e.Status != StatusResolved {
			t.Fatalf("diamond include edge = %v, want Resolved (not a cycle): %+v", e.Status, e)
		}
	}
	if res.Histogram.ByReason[ReasonCyclic] != 0 {
		t.Fatalf("cyclic count = %d, want 0 (a diamond re-include is not a cycle)", res.Histogram.ByReason[ReasonCyclic])
	}
}

// --- WalkTree: multiple independent entry points, no shared root -----------
func TestWalkTree_IndependentPortLikeUnits(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "portA/portfile.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/helpers.cmake)`)
	writeFile(t, root, "portA/helpers.cmake", `# leaf`)
	writeFile(t, root, "portB/portfile.cmake", `include(${CMAKE_CURRENT_LIST_DIR}/missing.cmake)`)

	res, err := WalkTree(context.Background(), root, root, []string{"portfile.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	if res.Histogram.Resolved != 1 || res.Histogram.Dangling != 1 {
		t.Fatalf("histogram = %+v, want {Resolved:1, Dangling:1}", res.Histogram)
	}
	if len(res.Edges) != 2 {
		t.Fatalf("edges = %d, want 2 (one per port)", len(res.Edges))
	}
}

// =====================================================================
// P3-8 shadow-path fixtures: the scanner's CANDIDATE exists but CMake's
// REAL target does not (or vice versa) — these are the fixtures a
// cross-family adversarial review (codex Sol, xhigh) demanded because the
// original mutation proof only exercised the "missing candidate ->
// Dangling" class, which every wrong-file-class bug below PASSES. Each
// test here was first run against the PRE-FIX code and confirmed to fail
// exactly as predicted (the falsification the review required) before the
// corresponding fix landed; they are now permanent regressions.
// =====================================================================

// P1-1: relative include() must resolve against CMAKE_CURRENT_SOURCE_DIR,
// not CMAKE_CURRENT_LIST_DIR. Shadow path: a decoy file exists at the WRONG
// (CMAKE_CURRENT_LIST_DIR-based) location; the CORRECT
// (CMAKE_CURRENT_SOURCE_DIR-based) location is empty, so real CMake would
// see this as Dangling. A resolver using the wrong base directory reports
// Resolved against the decoy instead.
func TestP1_1_RelativeIncludeResolvesAgainstCurrentSourceDirNotListDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(sub/helper.cmake)`)
	writeFile(t, root, "sub/helper.cmake", `include(local.cmake)`)
	writeFile(t, root, "sub/local.cmake", `# decoy: exists only at the WRONG (CMAKE_CURRENT_LIST_DIR) location`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var helperEdge *Edge
	for i := range res.Edges {
		if filepath.Base(res.Edges[i].FromFile) == "helper.cmake" {
			helperEdge = &res.Edges[i]
		}
	}
	if helperEdge == nil {
		t.Fatalf("expected an edge from helper.cmake; edges=%+v", res.Edges)
	}
	wantPath := filepath.Join(realpath(t, root), "local.cmake")
	if helperEdge.Status != StatusDangling || helperEdge.ResolvedPath != wantPath {
		t.Fatalf("got {%v,%q}, want {Dangling,%q} — real CMake resolves relative include() against CMAKE_CURRENT_SOURCE_DIR (root), not CMAKE_CURRENT_LIST_DIR (root/sub)",
			helperEdge.Status, helperEdge.ResolvedPath, wantPath)
	}
}

// P1-1 companion: the SAME bare-relative shape at the TOP LEVEL (not inside
// an included file) is unaffected, since CMAKE_CURRENT_SOURCE_DIR and
// CMAKE_CURRENT_LIST_DIR coincide there — proving the fix only changes
// behavior where the two variables actually diverge.
func TestP1_1_TopLevelBareRelativeUnaffected(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(sub/helper.cmake)`)
	writeFile(t, root, "sub/helper.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 || res.Edges[0].Status != StatusResolved {
		t.Fatalf("edges=%+v, want 1 Resolved edge (CMAKE_CURRENT_SOURCE_DIR == CMAKE_CURRENT_LIST_DIR at the top level)", res.Edges)
	}
}

// P1-2: ${CMAKE_CURRENT_LIST_DIR} inside a macro() body reflects the
// bottom-most INVOKING file at expansion time (CMake's own documented
// behavior), not the defining file — this package cannot know the invoker
// statically, so it must refuse rather than substitute the defining file's
// own directory. Shadow path: a decoy exists at the (wrong) defining-file
// location.
func TestP1_2_CurrentListDirInsideMacroBodyRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/sub/defs.cmake)`)
	writeFile(t, root, "sub/defs.cmake",
		"macro(load_target)\n"+
			"  include(${CMAKE_CURRENT_LIST_DIR}/target.cmake)\n"+
			"endmacro()\n")
	writeFile(t, root, "sub/target.cmake", `# decoy: exists only at the defining file's (possibly wrong) directory`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var macroEdge *Edge
	for i := range res.Edges {
		if filepath.Base(res.Edges[i].FromFile) == "defs.cmake" {
			macroEdge = &res.Edges[i]
		}
	}
	if macroEdge == nil {
		t.Fatalf("expected an edge from defs.cmake; edges=%+v", res.Edges)
	}
	if macroEdge.Status != StatusUnresolved || macroEdge.Reason != ReasonDeferredMacroContext {
		t.Fatalf("got {%v,%v}, want {Unresolved,deferred_macro_context}", macroEdge.Status, macroEdge.Reason)
	}
	if macroEdge.ResolvedPath != "" {
		t.Fatalf("deferred_macro_context edge must have no ResolvedPath (never computed): got %q", macroEdge.ResolvedPath)
	}
}

// P1-2 companion + the operator's requested blast-radius measurement: a
// function() body gates the same way, and a call OUTSIDE any macro/function
// in the SAME file is unaffected.
func TestP1_2_FunctionBodyAlsoGatesAndOutsideCallUnaffected(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"function(load_target)\n"+
			"  include(${CMAKE_CURRENT_LIST_DIR}/inside.cmake)\n"+
			"endfunction()\n"+
			"include(${CMAKE_CURRENT_LIST_DIR}/outside.cmake)\n")
	writeFile(t, root, "outside.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	insideEdge := edgeByLine(t, res, 2)
	if insideEdge.Status != StatusUnresolved || insideEdge.Reason != ReasonDeferredMacroContext {
		t.Fatalf("function-body edge = {%v,%v}, want {Unresolved,deferred_macro_context}", insideEdge.Status, insideEdge.Reason)
	}
	outsideEdge := edgeByLine(t, res, 4)
	if outsideEdge.Status != StatusResolved {
		t.Fatalf("outside-function edge = %v, want Resolved (must not be poisoned by an unrelated function() body)", outsideEdge.Status)
	}
}

// P1-3: an arbitrary *.cmake file scanned as an independent WalkTree
// discovery root has no verified CMAKE_CURRENT_SOURCE_DIR — a bare-relative
// argument there must refuse, not assume "its own directory". Shadow path:
// a decoy exists relative to the file's own (unverified) directory.
func TestP1_3_StandaloneRootBareRelativeRefusedUnverified(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "helpers/some_helper.cmake", `include(local.cmake)`)
	writeFile(t, root, "helpers/local.cmake", `# decoy: exists only relative to the INVENTED (unverified) source dir`)

	res, err := WalkTree(context.Background(), root, root, []string{"*.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	var edge *Edge
	for i := range res.Edges {
		if filepath.Base(res.Edges[i].FromFile) == "some_helper.cmake" {
			edge = &res.Edges[i]
		}
	}
	if edge == nil {
		t.Fatalf("expected an edge from some_helper.cmake; edges=%+v", res.Edges)
	}
	if edge.Status != StatusUnresolved || edge.Reason != ReasonUnverifiedSourceDir {
		t.Fatalf("got {%v,%v}, want {Unresolved,unverified_source_dir}", edge.Status, edge.Reason)
	}
}

// P1-3 companion: the SAME bare-relative shape from a GENUINE CMakeLists.txt
// root (a verified context) still resolves normally — proving the fix
// narrowly targets unverified contexts, not bare-relative resolution as a
// whole.
func TestP1_3_CMakeListsRootBareRelativeStillResolves(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(local.cmake)`)
	writeFile(t, root, "local.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 || res.Edges[0].Status != StatusResolved {
		t.Fatalf("edges=%+v, want 1 Resolved edge (a CMakeLists.txt root's context is verified)", res.Edges)
	}
}

// P1-3: add_subdirectory() resolved from a verified context correctly
// establishes a FRESH verified context for the child, regardless of how the
// parent's own context was established (absolute-variable or bare-relative
// under an already-verified parent).
func TestP1_3_AddSubdirectoryEstablishesFreshVerifiedContext(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `add_subdirectory(child)`)
	writeFile(t, root, "child/CMakeLists.txt", `include(local.cmake)`)
	writeFile(t, root, "child/local.cmake", `# leaf, relative to child's OWN (freshly verified) source dir`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var childEdge *Edge
	for i := range res.Edges {
		if filepath.Base(res.Edges[i].FromFile) == "CMakeLists.txt" && strings.Contains(filepath.ToSlash(res.Edges[i].FromFile), "/child") {
			childEdge = &res.Edges[i]
		}
	}
	if childEdge == nil {
		t.Fatalf("expected an edge from child/CMakeLists.txt; edges=%+v", res.Edges)
	}
	if childEdge.Status != StatusResolved {
		t.Fatalf("child's bare-relative include = %v, want Resolved (add_subdirectory establishes a fresh verified context)", childEdge.Status)
	}
}

// P1-4: $CACHE{...} must be refused before any stat is attempted, same
// bucket as $ENV{...} — never stat'd as literal text.
func TestP1_4_CacheVariableRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include($CACHE{P}.cmake)`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	if e.Status != StatusUnresolved || e.Reason != ReasonNonStaticVariable {
		t.Fatalf("got {%v,%v}, want {Unresolved,non_static_variable}", e.Status, e.Reason)
	}
}

// P1-4: an argument containing a raw backslash is refused (escape sequences
// are never decoded). The CORRECT (space, no backslash) file exists on
// disk; a resolver that stats the raw backslash-bearing text would never
// find it anyway on Windows (backslash is the path separator), but the
// point is it must not even TRY — it must refuse outright.
func TestP1_4_BackslashEscapeSequenceRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include("foo\ bar.cmake")`)
	writeFile(t, root, "foo bar.cmake", `# the file CMake would actually open, after decoding \  as a literal space`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	if e.Status != StatusUnresolved || e.Reason != ReasonParseError {
		t.Fatalf("got {%v,%v}, want {Unresolved,parse_error} — escape sequences must be refused, not decoded or stat'd raw", e.Status, e.Reason)
	}
	if e.ResolvedPath != "" {
		t.Fatalf("must never compute a path for a backslash-bearing argument: got %q", e.ResolvedPath)
	}
}

// P1-4: CMake bracket-argument syntax is recognized (so it doesn't get
// mis-parsed as a bare token) but its content is never extracted as a path.
func TestP1_4_BracketArgumentRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "include([[some/path.cmake]])\n")
	writeFile(t, root, "some/path.cmake", `# decoy: must never be reached via bracket-argument content`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	if e.Status != StatusUnresolved || e.Reason != ReasonParseError {
		t.Fatalf("got {%v,%v}, want {Unresolved,parse_error}", e.Status, e.Reason)
	}
}

// P1-5: message(include(existing.cmake)) is a single, syntactically legal
// unquoted argument to message() (balanced nested parens), NOT an include()
// directive. existing.cmake genuinely exists — proving the false positive
// is about misinterpreting syntax, not a coincidental dangling miss.
func TestP1_5_NestedCommandInBalancedParensNotMisreadAsDirective(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `message(include(existing.cmake))`)
	writeFile(t, root, "existing.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 0 {
		t.Fatalf("edges = %d, want 0: %+v", len(res.Edges), res.Edges)
	}
}

// P1-5: message(if(fake)) must not corrupt the if()/endif() nesting counter
// used for Edge.Conditional — a REAL if()/endif() pair afterward must still
// correctly gate a REAL call.
func TestP1_5_FakeNestedIfDoesNotCorruptConditionalTracking(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"message(if(fake))\n"+
			"if(REAL_FLAG)\n"+
			"  include(${CMAKE_CURRENT_LIST_DIR}/real_conditional.cmake)\n"+
			"endif()\n")
	writeFile(t, root, "real_conditional.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	if e.Status != StatusResolved || !e.Conditional {
		t.Fatalf("got {%v,Conditional:%v}, want {Resolved,Conditional:true} — the fake if( inside message(...) must not corrupt the REAL if()/endif() nesting count", e.Status, e.Conditional)
	}
}

// P1-5: escape parity — message("x\\") must correctly recognize the
// backslash-backslash as ONE escaped backslash (not "escaped quote-to-come"),
// so the string closes normally and a REAL command afterward is still
// discovered. A naive "look at only the immediately preceding byte" check
// gets this wrong and can swallow the next real command.
func TestP1_5_EscapeParityDoesNotSwallowNextCommand(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		`message("x\\")`+"\n"+
			"include(${CMAKE_CURRENT_LIST_DIR}/real.cmake)\n")
	writeFile(t, root, "real.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1 (the real include() after message(\"x\\\\\") must still be discovered): %+v", len(res.Edges), res.Edges)
	}
	if res.Edges[0].Status != StatusResolved {
		t.Fatalf("edge = %v, want Resolved", res.Edges[0].Status)
	}
}

// P1-5: bracket comments are matched to their ACTUAL closing bracket, not
// just stripped on the opening line — a command-name-shaped substring
// spanning multiple lines inside one must not be discovered.
func TestP1_5_BracketCommentMatchedAcrossMultipleLines(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt",
		"#[[ this is a bracket comment\n"+
			"include(should_not_be_found.cmake)\n"+
			"still inside the comment\n"+
			"]]\n"+
			"include(${CMAKE_CURRENT_LIST_DIR}/real.cmake)\n")
	writeFile(t, root, "should_not_be_found.cmake", `# decoy`)
	writeFile(t, root, "real.cmake", `# leaf`)

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1 (only the include() AFTER the bracket comment): %+v", len(res.Edges), res.Edges)
	}
	if filepath.Base(res.Edges[0].ResolvedPath) != "real.cmake" {
		t.Fatalf("resolved = %q, want real.cmake", res.Edges[0].ResolvedPath)
	}
}

// P1-6: MaxNodes must gate independent-root ADMISSION, not just edge
// traversal within an already-admitted root, and the refusal must be
// visible.
func TestP1_6_NodeCapGatesRootAdmissionAndIsVisible(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.cmake", `# leaf a`)
	writeFile(t, root, "b.cmake", `# leaf b`)

	res, err := WalkTree(context.Background(), root, root, []string{"*.cmake"}, Options{MaxDepth: DefaultMaxDepth, MaxNodes: 1, MaxFileBytes: DefaultMaxFileBytes})
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("files scanned = %d, want 1: %v", len(res.Files), res.Files)
	}
	if !res.NodeCapTruncated || res.RootsSkippedByNodeCap != 1 {
		t.Fatalf("NodeCapTruncated=%v RootsSkippedByNodeCap=%d, want {true,1}", res.NodeCapTruncated, res.RootsSkippedByNodeCap)
	}
}

// P2-7: a byte-cap-exceeding file is recorded in Result.UnscannedFiles — its
// own outgoing edges are known-missing, not silently absent with no trace.
// The edge leading TO the oversized file remains Resolved (the filesystem
// fact "it exists" is still true).
func TestP2_7_ByteCapSkipIsVisibleNotSilent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", `include(${CMAKE_CURRENT_LIST_DIR}/huge.cmake)`)
	huge := make([]byte, 200)
	for i := range huge {
		huge[i] = '#'
	}
	huge = append(huge, []byte("\ninclude(${CMAKE_CURRENT_LIST_DIR}/never_seen.cmake)\n")...)
	if err := os.WriteFile(filepath.Join(root, "huge.cmake"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "never_seen.cmake", `# leaf`)

	opts := Options{MaxDepth: DefaultMaxDepth, MaxNodes: DefaultMaxNodes, MaxFileBytes: 50}
	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 || res.Edges[0].Status != StatusResolved {
		t.Fatalf("edges=%+v, want 1 Resolved edge (huge.cmake exists; only its OWN content is unreadable)", res.Edges)
	}
	if len(res.UnscannedFiles) != 1 || filepath.Base(res.UnscannedFiles[0].Path) != "huge.cmake" {
		t.Fatalf("UnscannedFiles=%+v, want exactly 1 entry for huge.cmake", res.UnscannedFiles)
	}
	if res.UnscannedFiles[0].Reason == "" {
		t.Fatalf("UnscannedFiles[0].Reason must not be empty")
	}
}

// --- Mutation proof retargeted to the WRONG-FILE class (P3-8) --------------
//
// The original mutation proof (TestMutationProof_DanglingPathNeverReportedResolved,
// above) only proves a resolver that forgets to check existence reports
// Dangling instead of Resolved for a MISSING candidate — every wrong-base-
// directory bug in this file passes that test unchanged (it still checks
// existence; it just checks the WRONG path). This mutation targets exactly
// that class: temporarily reintroduce the P1-1 bug (resolve include()
// against CMAKE_CURRENT_LIST_DIR instead of CMAKE_CURRENT_SOURCE_DIR) and
// confirm TestP1_1_RelativeIncludeResolvesAgainstCurrentSourceDirNotListDir
// fails against it. This is exercised manually during review (mutate
// classifyArg's `p = filepath.Join(ctx.dir, p)` to use listDir instead,
// rerun -run TestP1_1_, confirm FAIL, revert) — recorded here as a
// permanent comment pointing at the exact test so a future reviewer can
// repeat the mutation without guessing which test covers it.

// =====================================================================
// Pre-submission cross-family review, round 2 (F16/F17/F18/F22/F27).
// Every test below was first run against the PRE-FIX code and confirmed
// to fail — the mutation evidence is recorded in the review report.
// =====================================================================

// F18: a RECOGNIZED bracket-argument opener with no matching close must not
// be downgraded to a literal '[' — doing so hands the payload's own bytes
// back to the scanner as executable syntax. In the fixture below the
// payload's ')' would close message(), after which include(real.cmake) would
// be read as a genuine top-level call and emitted as a REAL edge to a file
// that CMake would never process, because everything from `[=[` onwards is
// uninterpreted text with no terminator.
func TestF18_UnterminatedBracketArgumentDoesNotLeakPayloadAsSyntax(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "message([=[payload ) include(real.cmake)\n")
	// real.cmake EXISTS: if the scanner mis-parses, the fabricated edge would
	// come back StatusResolved, which is the worst possible failure mode —
	// a confident, verifiable-looking claim about a file CMake never reads.
	writeFile(t, root, "real.cmake", "# decoy: must never be reached\n")

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 0 {
		t.Fatalf("edges = %d, want 0 — an unterminated bracket argument must stop interpretation, "+
			"never expose its payload as syntax: %+v", len(res.Edges), res.Edges)
	}
	for _, f := range res.Files {
		if filepath.Base(f) == "real.cmake" {
			t.Fatalf("real.cmake was scanned; it is inside an unterminated bracket argument and is not a real include: %v", res.Files)
		}
	}
}

// F18 (companion): a '[' that is NOT a bracket opener must still be treated
// as an ordinary literal. This is the discrimination the fix turns on — if
// it regressed to "any '[' is malformed", real CMake using '[' in an
// unquoted argument would stop being scanned.
func TestF18_LiteralBracketIsNotMistakenForAnUnterminatedOpener(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "message(a[b)\ninclude(${CMAKE_CURRENT_LIST_DIR}/real.cmake)\n")
	writeFile(t, root, "real.cmake", "# leaf\n")

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 || res.Edges[0].Status != StatusResolved {
		t.Fatalf("edges = %+v, want exactly 1 Resolved edge: a bare '[' is a literal, not a bracket opener", res.Edges)
	}
}

// F17: os.Stat failing for a reason OTHER than "not exist" proves nothing
// about absence, so it must never be reported as StatusDangling — whose
// contract is VERIFIED absence.
//
// The failure is injected via a path that cannot exist as a NAME on either
// platform rather than via permissions, so the test is deterministic and
// does not depend on running as a non-privileged user. A NUL byte in a path
// makes the syscall fail with EINVAL/ENOENT-adjacent errors that are NOT
// fs.ErrNotExist on Windows (ERROR_INVALID_NAME).
func TestF17_NonAbsenceStatErrorIsUnresolvedNotDangling(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ERROR_INVALID_NAME shape is Windows-specific; the POSIX equivalent needs a permission fixture")
	}
	root := t.TempDir()
	// A trailing space + a reserved character produce ERROR_INVALID_NAME on
	// Windows, which is emphatically not "the file is absent".
	writeFile(t, root, "CMakeLists.txt", "include(${CMAKE_CURRENT_LIST_DIR}/bad>name.cmake)\n")

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1: %+v", len(res.Edges), res.Edges)
	}
	e := res.Edges[0]
	if e.Status == StatusDangling {
		t.Fatalf("edge = dangling, want unresolved/target_unreadable — dangling asserts VERIFIED absence, "+
			"but os.Stat failed with something other than fs.ErrNotExist: %+v", e)
	}
	if e.Status != StatusUnresolved || e.Reason != ReasonTargetUnreadable {
		t.Fatalf("edge = {%v,%v}, want {Unresolved,target_unreadable}: %+v", e.Status, e.Reason, e)
	}
}

// F17 (companion): a genuinely absent target must STILL be dangling. The fix
// narrows dangling; it must not eliminate it.
func TestF17_GenuinelyAbsentTargetIsStillDangling(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "include(${CMAKE_CURRENT_LIST_DIR}/gone.cmake)\n")

	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 || res.Edges[0].Status != StatusDangling {
		t.Fatalf("edges = %+v, want 1 Dangling edge (fs.ErrNotExist IS verified absence)", res.Edges)
	}
}

// F16: a subtree that could not be enumerated must be recorded as incomplete
// coverage. Without this the wrapper returns an apparently-complete graph
// with no UnscannedFiles entry, and a caller cannot tell "this subtree held
// no CMake files" from "we were not allowed to look".
//
// The subtree is made genuinely unreadable (denied ACL on Windows, mode 000
// on POSIX) and the INSTRUMENT IS VALIDATED FIRST: if a direct os.ReadDir of
// that directory still succeeds, the denial did not take effect and the test
// SKIPS rather than passing vacuously. A test that silently stops exercising
// the path it names is worse than no test.
func TestF16_UnenumerableSubtreeIsRecordedAsIncompleteCoverage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "top.cmake", "# leaf\n")
	sub := filepath.Join(root, "denied")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "denied/inner.cmake", "# a matching file we must never claim does not exist\n")
	denyDirectoryListing(t, sub)

	// Instrument validation: prove the denial actually bites before asserting
	// anything about how the walker reacts to it.
	if _, err := os.ReadDir(sub); err == nil {
		t.Skip("directory-listing denial did not take effect on this host; nothing to exercise")
	}

	res, err := WalkTree(context.Background(), root, root, []string{"*.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	// top.cmake must still be scanned: one bad subtree never aborts the walk.
	if len(res.Files) != 1 || filepath.Base(res.Files[0]) != "top.cmake" {
		t.Fatalf("files = %v, want just top.cmake — a single unreadable subtree must not abort the walk", res.Files)
	}
	var sawHole bool
	for _, u := range res.UnscannedFiles {
		if u.Reason == CoverageEnumerateFailed {
			sawHole = true
		}
	}
	if !sawHole {
		t.Fatalf("UnscannedFiles = %+v, want an enumerate_failed entry — without it this result is an "+
			"apparently-complete graph that silently omits denied/inner.cmake", res.UnscannedFiles)
	}
}

// denyDirectoryListing removes the current user's ability to list dir, and
// registers a cleanup that restores it (registered AFTER t.TempDir's own
// cleanup, so it runs BEFORE it and the temp tree can still be removed).
func denyDirectoryListing(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Deny this account read access on the directory itself. Uses the
		// well-known Everyone SID literal (locale-proof; a display name
		// varies by system language).
		run := func(args ...string) error {
			return exec.Command("icacls", args...).Run()
		}
		if err := run(dir, "/deny", "*S-1-1-0:(RX)"); err != nil {
			t.Skipf("icacls deny unavailable on this host: %v", err)
		}
		t.Cleanup(func() {
			_ = run(dir, "/remove:d", "*S-1-1-0")
		})
		return
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skipf("chmod 000 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// F16 (the load-bearing half): a failure to enumerate the ROOT ITSELF must
// TERMINATE with an error. Returning an empty, ok-looking graph would assert
// "this tree contains no CMake files", which is precisely the confident
// answer we have no evidence for.
func TestF16_RootEnumerationFailureTerminatesWithError(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "no-such-directory")

	_, err := WalkTree(context.Background(), missing, missing, []string{"*.cmake"}, DefaultOptions())
	if err == nil {
		t.Fatal("WalkTree returned nil error for an unenumerable root — an empty graph here is a false " +
			"\"no CMake files found\" claim, not an answer")
	}
}

// F22: a root must be rejected on the workspace boundary BEFORE it is
// stat'ed or read. An enumerated *.cmake entry inside the tree may be a
// symlink whose target lives outside workspace_root; opening it to discover
// that would already have performed the read the boundary exists to prevent.
func TestF22_SymlinkedRootEscapingWorkspaceIsRefusedBeforeAnyRead(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.cmake")
	if err := os.WriteFile(secret, []byte("include(${CMAKE_CURRENT_LIST_DIR}/leaked.cmake)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "innocent.cmake")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("cannot create symlink on this host (needs Developer Mode or admin on Windows): %v", err)
	}

	res, err := WalkTree(context.Background(), workspace, workspace, []string{"*.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	for _, f := range res.Files {
		if strings.Contains(strings.ToLower(filepath.ToSlash(f)), "/outside/") {
			t.Fatalf("scanned a file outside the workspace boundary: %v", res.Files)
		}
	}
	if len(res.Edges) != 0 {
		t.Fatalf("edges = %+v, want 0: the escaping root must never be read at all", res.Edges)
	}
	var sawRefusal bool
	for _, u := range res.UnscannedFiles {
		if u.Reason == CoverageRootOutsideWorkspace {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatalf("UnscannedFiles = %+v, want a root_outside_workspace entry — a silent refusal is still a coverage hole", res.UnscannedFiles)
	}
}

// F27: MaxNodes bounds ADMISSION, not enumeration, so it could never bound
// the memory spent listing candidate roots. MaxRoots does, and the fact that
// it tripped is reported rather than left to look like a complete scan.
func TestF27_RootEnumerationIsBoundedAndTheCapIsVisible(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.cmake", "b.cmake", "c.cmake", "d.cmake"} {
		writeFile(t, root, name, "# leaf\n")
	}

	opts := DefaultOptions()
	opts.MaxRoots = 2
	res, err := WalkTree(context.Background(), root, root, []string{"*.cmake"}, opts)
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	if !res.RootEnumerationCapped {
		t.Fatalf("RootEnumerationCapped = false, want true: enumeration stopped at MaxRoots=2 but the result " +
			"claims to have seen the whole tree")
	}
	if len(res.Files) != 2 {
		t.Fatalf("files scanned = %d (%v), want 2 — enumeration must STOP at the cap, not enumerate all four and admit two",
			len(res.Files), res.Files)
	}
	var sawCap bool
	for _, u := range res.UnscannedFiles {
		if u.Reason == CoverageRootEnumerationCapped {
			sawCap = true
		}
	}
	if !sawCap {
		t.Fatalf("UnscannedFiles = %+v, want a root_enumeration_capped entry", res.UnscannedFiles)
	}
}

// F20 (cmakegraph half): WalkTree must honour a workspaceRoot that is a
// PARENT of root. Pinning the workspace to root made every include reaching
// a sibling directory of the caller's real workspace look like an escape.
// It covers BOTH jobs workspaceRoot does, because the finding names both:
// the escape BOUNDARY (the ../ include) and the ${CMAKE_SOURCE_DIR} value
// (the absolute include). Pinning the workspace to root breaks each in a
// different way, so a fix that only threaded one through would still fail
// here.
func TestF20_WalkTreeHonoursAParentWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, workspace, "shared/helper.cmake", "# leaf\n")
	writeFile(t, workspace, "shared/other.cmake", "# leaf\n")
	writeFile(t, workspace, "ports/viaboundary.cmake",
		"include(${CMAKE_CURRENT_LIST_DIR}/../shared/helper.cmake)\n")
	writeFile(t, workspace, "ports/viasourcedir.cmake",
		"include(${CMAKE_SOURCE_DIR}/shared/other.cmake)\n")
	scanRoot := filepath.Join(workspace, "ports")

	// Control: workspace pinned to the scan root — which is what the wrapper
	// used to force regardless of what the caller passed.
	pinned, err := WalkTree(context.Background(), scanRoot, scanRoot, []string{"*.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree (workspace pinned to root): %v", err)
	}
	for _, e := range pinned.Edges {
		if e.Status == StatusResolved {
			t.Fatalf("control case produced a Resolved edge; with the workspace pinned to ports/ neither "+
				"include can resolve: %+v", pinned.Edges)
		}
	}
	var sawOutside bool
	for _, e := range pinned.Edges {
		if e.Reason == ReasonOutsideWorkspace {
			sawOutside = true
		}
	}
	if !sawOutside {
		t.Fatalf("control case = %+v, want the ../shared include reported outside_workspace", pinned.Edges)
	}

	// The fix: the SUPPLIED parent workspace is used for both jobs.
	res, err := WalkTree(context.Background(), scanRoot, workspace, []string{"*.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree (parent workspace): %v", err)
	}
	if len(res.Edges) != 2 {
		t.Fatalf("edges = %d, want 2: %+v", len(res.Edges), res.Edges)
	}
	for _, e := range res.Edges {
		if e.Status != StatusResolved {
			t.Fatalf("edge %+v = %v, want Resolved: workspace_root must be honoured as BOTH the escape "+
				"boundary and the ${CMAKE_SOURCE_DIR} value", e, e.Status)
		}
	}
}

// Cancellation: a canceled walk must RETURN the error, never hand back the
// partial graph it happened to accumulate as if it were complete.
func TestCanceledWalkReturnsErrorNotAPartialGraph(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "include(${CMAKE_CURRENT_LIST_DIR}/a.cmake)\n")
	writeFile(t, root, "a.cmake", "# leaf\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Walk(ctx, filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions()); err == nil {
		t.Fatal("Walk returned nil error for a canceled context")
	}
	if _, err := WalkTree(ctx, root, root, []string{"*.cmake"}, DefaultOptions()); err == nil {
		t.Fatal("WalkTree returned nil error for a canceled context")
	}
}

// The coverage reason is a CLOSED enum, not the free-form error string it
// used to be — a caller can switch on it.
func TestByteCapCoverageCarriesAClosedReasonAndADetail(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "include(${CMAKE_CURRENT_LIST_DIR}/huge.cmake)\n")
	huge := strings.Repeat("#", 200) + "\n"
	if err := os.WriteFile(filepath.Join(root, "huge.cmake"), []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := DefaultOptions()
	opts.MaxFileBytes = 50
	res, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.UnscannedFiles) != 1 {
		t.Fatalf("UnscannedFiles = %+v, want exactly 1", res.UnscannedFiles)
	}
	if res.UnscannedFiles[0].Reason != CoverageByteCapExceeded {
		t.Fatalf("Reason = %q, want %q (closed enum, never a free-form error string)",
			res.UnscannedFiles[0].Reason, CoverageByteCapExceeded)
	}
	if res.UnscannedFiles[0].Detail == "" {
		t.Fatal("Detail must carry the underlying error text for a human")
	}
}
