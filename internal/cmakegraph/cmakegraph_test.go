package cmakegraph

import (
	"os"
	"path/filepath"
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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
		res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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
	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, opts)
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
	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, opts)
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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
	// The FIRST occurrence ("add_subdirectory(src/Tutorials/" — its `(` is
	// unambiguously inside the quotes) must be caught. Accept either 1 or 2
	// depending on whether the second (commented + quoted) occurrence's `(`
	// is also inside the quote span; assert it is AT LEAST 1 so the guard is
	// proven to fire, without over-constraining on a construct this package
	// doesn't otherwise need to disambiguate further.
	if res.SkippedInStringLiteral < 1 {
		t.Fatalf("SkippedInStringLiteral = %d, want >= 1", res.SkippedInStringLiteral)
	}
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

	narrow, err := WalkTree(root, []string{"portfile.cmake"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree (narrow): %v", err)
	}
	if len(narrow.Edges) != 0 {
		t.Fatalf("narrow (portfile.cmake-only) scan edges = %d, want 0 — the toolchain file is unreachable from any portfile.cmake: %+v", len(narrow.Edges), narrow.Edges)
	}

	wide, err := WalkTree(root, []string{"*.cmake", "CMakeLists.txt"}, DefaultOptions())
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

	res, err := WalkTree(root, []string{"*.cmake"}, DefaultOptions())
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

	res, err := Walk(filepath.Join(root, "CMakeLists.txt"), root, DefaultOptions())
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

	res, err := WalkTree(root, []string{"portfile.cmake"}, DefaultOptions())
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
