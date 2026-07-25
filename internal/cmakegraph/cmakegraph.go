// Package cmakegraph statically resolves the CMake include()/add_subdirectory()
// graph rooted at a file or directory tree, WITHOUT ever invoking cmake.
//
// # Why this exists, and why it is NOT wired into mcphub
//
// This package has ZERO importers in this repository by design. It is a
// standalone diagnostic tool, not a running-system dependency: nothing in
// mcp-local-hub's manifest, backend dispatch, or MCP tool catalog references
// it. It was written after protocol-level measurement showed that wiring
// `cmake` into the `mcp-language-server` backend (which fronts neocmakelsp)
// would advertise MCP tools (`definition`, `references`, `rename_symbol`)
// that error out (`workspace/symbol` is unimplemented server-side, -32601),
// while the capabilities neocmakelsp actually has (completion, documentSymbol,
// in-file goto) are not exposed as MCP tools by that wrapper at all. That is
// a worse experience than nothing, so it was reverted. This package instead
// answers the underlying question directly and safely: given a CMake project
// tree (including a vcpkg-style overlay-ports tree that has no root
// CMakeLists.txt), how much of its include()/add_subdirectory() graph can be
// resolved by pure lexical scanning, and where does it break down?
//
// # Read-only, non-executing
//
// This package NEVER writes a file, NEVER invokes the `cmake` binary or any
// other subprocess, and NEVER shells out. It is a text scanner over
// operator-controlled CMake source, so every traversal is bounded (depth,
// total node count, per-file byte size) and every resolved path is checked
// against a caller-supplied workspace boundary (including symlink
// resolution) before being trusted.
//
// # Tri-state resolution, never a false positive
//
// Every include()/add_subdirectory() call site becomes one Edge with one of
// three Status values:
//
//   - StatusResolved: a concrete path was computed AND os.Stat confirms the
//     target exists (a file for include(), or a directory containing its own
//     CMakeLists.txt for add_subdirectory()).
//   - StatusDangling: a concrete path was computed but the target is absent.
//     This is a real finding (a broken include), not a scanner failure.
//   - StatusUnresolved: the scanner could not safely compute a path at all.
//     Reason names exactly why (see the Reason constants) — this is the
//     scanner declining to guess rather than reporting a wrong "resolved".
//
// The design goal is: it is always safe to trust a StatusResolved edge, and
// StatusUnresolved always means "the scanner does not know", never "the
// scanner assumed and might be wrong".
//
// # What is (and is NOT) lexically substituted
//
//   - ${CMAKE_CURRENT_LIST_DIR} is ALWAYS safe to substitute: by CMake's own
//     semantics it is exactly the directory of the file containing the call,
//     with no dependency on how that file was reached (include() chain or
//     add_subdirectory() chain). No CMake evaluation is needed for it.
//   - ${CMAKE_SOURCE_DIR} is a project-wide constant (the top of the whole
//     tree) fixed once at the very top and invariant regardless of nesting
//     depth, so it is substituted with the caller-supplied workspace root
//     whenever a root is supplied — which Walk/WalkTree always require.
//   - ${CMAKE_CURRENT_SOURCE_DIR} is DELIBERATELY NEVER substituted, even
//     though this package tracks an internal notion of "current source
//     directory" to resolve bare (variable-free) add_subdirectory() targets.
//     Real CMake's CMAKE_CURRENT_SOURCE_DIR changes only on add_subdirectory()
//     and is INHERITED (unchanged) across include() calls — so it diverges
//     from CMAKE_CURRENT_LIST_DIR precisely inside an included file, and its
//     true value also depends on whether the call site is reached through an
//     add_subdirectory() chain the scanner does not fully evaluate (e.g. a
//     call inside an unevaluated conditional, or inside a helper file that is
//     itself include()-d from more than one place). This package's own
//     internal sourceDir bookkeeping is a best-effort approximation good
//     enough to resolve the common bare-relative add_subdirectory(name) case;
//     it is intentionally NEVER exposed as a substitution for a LITERAL
//     "${CMAKE_CURRENT_SOURCE_DIR}" token in scanned text, because doing so
//     would require accurately modelling CMake's directory-scope stack across
//     arbitrary nesting and conditionals — exactly the kind of guess this
//     package refuses to make. Any occurrence of the literal token is
//     classified StatusUnresolved / ReasonNonStaticVariable.
//   - Any other ${...} variable, $ENV{...} reference, or $<...> generator
//     expression is never evaluated (see the Reason constants below).
//
// # Conditional calls: the path is resolved anyway
//
// A call textually nested inside an if()/elseif()/else() block answers two
// INDEPENDENT questions: "where does this point?" (fully statically
// computable from the text, same as an unconditional call) and "will it
// execute?" (genuinely unknown without evaluating CMake). This package only
// answers the first question, via Status — Edge.Conditional records the
// second as a plain fact, never gating resolution. A conditional edge is
// Resolved exactly when its path is computed AND the target exists, same
// rule as an unconditional edge; Conditional does not change Status, and
// Status never implies whether the branch is actually taken. Read
// Conditional: true as "this edge exists in the source, but a cmake trace is
// required to know if it is ever reached at configure time."
//
// # Known, documented simplifications
//
//   - Argument parsing takes only the FIRST whitespace/quote-delimited token
//     of an include()/add_subdirectory() call as the path/module argument,
//     ignoring trailing keyword arguments (OPTIONAL, RESULT_VARIABLE,
//     EXCLUDE_FROM_ALL, add_subdirectory's optional binary-dir argument).
//     This matches the dominant real-world call shape.
//   - Conditional detection (Edge.Conditional) is a simple if()/endif()
//     nesting counter per file; it does not model elseif()/else() branch
//     selection (any call nested inside ANY if()...endif() block is flagged
//     conditional, regardless of which branch it is textually in), it is NOT
//     transitive (a call is flagged only by an if() in its OWN file — if
//     that file was itself reached via a conditional edge, this package does
//     not propagate that fact onto the file's own unconditional-looking
//     calls), and it does NOT detect that a call sits inside a
//     function()/macro() body (whose execution is also deferred) — all
//     documented, out-of-scope limitations.
//   - Bracket-argument syntax (CMake's `[[...]]` / `[=[...]=]` long strings)
//     is not recognized; only unquoted and double-quoted arguments are.
//   - A file-read failure AFTER a successful Stat (e.g. permission denied on
//     open, a TOCTOU race) leaves the edge that led to it Resolved (the
//     filesystem fact "the target exists" remains true) but the scanner
//     simply does not descend further into it — no further edges are
//     discovered from that file. This is different from a byte-cap trip,
//     which behaves identically for the same reason: bounding the SCAN, not
//     re-litigating whether the target exists.
//   - CONFIRMED via a real read-only run against a vcpkg-style overlay-ports
//     tree: a command name can appear INSIDE an unrelated string-literal
//     ARGUMENT to some other command — e.g. vcpkg_replace_string(...)
//     commonly embeds literal text like "add_subdirectory(python)" as a
//     find/replace pattern applied to a DIFFERENT file, not as a real call.
//     This package reuses its existing quote-tracking (the same primitive
//     that already strips `#` comments outside quotes) to detect this: a
//     match whose opening `(` falls inside a double-quoted span is skipped
//     entirely — no Edge is emitted for it — and counted in
//     Result.SkippedInStringLiteral for transparency. This guard inherits
//     the same scope as the rest of this package's quote handling (simple
//     `\"`-escaped double quotes only; CMake bracket-argument strings
//     `[[...]]`/`[=[...]=]` are not modeled, so a command-name-like
//     substring inside ONE of those is not caught by this guard).
package cmakegraph

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Status is the tri-state resolution outcome of one include()/add_subdirectory() edge.
type Status int

const (
	// StatusUnknown is the zero value and must never appear in a returned Edge.
	StatusUnknown Status = iota
	// StatusResolved means a concrete path was computed AND confirmed to exist.
	StatusResolved
	// StatusDangling means a concrete path was computed but the target is absent.
	StatusDangling
	// StatusUnresolved means no concrete path could safely be computed; see Reason.
	StatusUnresolved
)

func (s Status) String() string {
	switch s {
	case StatusResolved:
		return "resolved"
	case StatusDangling:
		return "dangling"
	case StatusUnresolved:
		return "unresolved"
	default:
		return "unknown"
	}
}

// Reason is populated only when Status == StatusUnresolved. This is a CLOSED
// set — callers may safely switch on it exhaustively.
type Reason string

const (
	// ReasonNonStaticVariable: the argument references a ${...} variable (or
	// $ENV{...}) this package does not know is safe to substitute — including
	// the deliberately-unresolved ${CMAKE_CURRENT_SOURCE_DIR}.
	ReasonNonStaticVariable Reason = "non_static_variable"
	// ReasonGeneratorExpression: the argument contains a $<...> generator
	// expression, which is only meaningful at CMake generate time.
	ReasonGeneratorExpression Reason = "generator_expression"
	// ReasonModuleNameNotPath: an include() argument with no path separator
	// and no .cmake suffix — a bare CMake module name resolved via
	// CMAKE_MODULE_PATH, which this package does not have access to.
	ReasonModuleNameNotPath Reason = "module_name_not_path"
	// ReasonParseError: the call's own argument syntax could not be safely
	// extracted (empty argument, unterminated quote).
	ReasonParseError Reason = "parse_error"
	// ReasonCyclic: the resolved target is already an ancestor of this call
	// site on the current traversal path — a genuine circular reference.
	ReasonCyclic Reason = "cyclic"
	// ReasonDepthLimit: the target is a genuinely new, resolvable node, but
	// following it would exceed the configured depth or total-node bound.
	ReasonDepthLimit Reason = "depth_limit"
	// ReasonOutsideWorkspace: the computed path, after symlink resolution,
	// escapes the caller-supplied workspace root.
	ReasonOutsideWorkspace Reason = "outside_workspace"
)

// EdgeKind distinguishes the two directives this package tracks.
type EdgeKind string

const (
	EdgeInclude         EdgeKind = "include"
	EdgeAddSubdirectory EdgeKind = "add_subdirectory"
)

// Edge is one include()/add_subdirectory() call site discovered while
// scanning a file, plus its resolution outcome.
type Edge struct {
	Kind   EdgeKind
	// FromFile is the absolute, symlink-resolved path of the file containing the call.
	FromFile string
	// Line is the 1-based line number of the call's opening `(` in FromFile.
	Line int
	// RawArg is the first argument token as written in the source, BEFORE
	// any variable substitution (e.g. "${CMAKE_CURRENT_LIST_DIR}/foo.cmake").
	RawArg string
	Status Status
	// Reason is set only when Status == StatusUnresolved.
	Reason Reason
	// ResolvedPath is the computed absolute path. Set whenever a concrete
	// path was computed at all — i.e. for StatusResolved, StatusDangling, and
	// the StatusUnresolved/ReasonOutsideWorkspace and
	// StatusUnresolved/ReasonDepthLimit cases (where a path WAS computed, but
	// the scanner chose not to trust or not to follow it). Empty for every
	// other unresolved reason, since no path was ever computed.
	ResolvedPath string
	// Conditional is true when this call is textually nested inside an
	// if()/elseif()/else() block in ITS OWN file (see the package doc's
	// "Conditional calls" section). It is independent of Status: a
	// conditional edge is Resolved/Dangling by the exact same os.Stat rule as
	// an unconditional one. Conditional == true means only "whether this
	// executes at configure time is unknown", never "the path is unknown".
	Conditional bool
}

// Histogram tallies edges by outcome. This is the operator-facing go/no-go metric.
type Histogram struct {
	Resolved   int
	Dangling   int
	Unresolved int
	ByReason   map[Reason]int
}

// Result is the full output of one Walk or WalkTree call.
type Result struct {
	// Root is the absolute path this walk started from (a file for Walk's
	// startFile, or the supplied directory for WalkTree).
	Root string
	// WorkspaceRoot is the boundary + ${CMAKE_SOURCE_DIR} value used.
	WorkspaceRoot string
	Edges         []Edge
	// Files lists every file this package actually opened and scanned
	// (deduplicated, sorted, absolute+symlink-resolved paths).
	Files     []string
	Histogram Histogram
	// SkippedInStringLiteral counts include()/add_subdirectory()-shaped
	// matches whose opening `(` fell inside a double-quoted string — a
	// command name embedded in an unrelated string argument (e.g.
	// vcpkg_replace_string(...)'s find/replace text), not a real directive.
	// These produce NO Edge at all (see the package doc's D3 note); this
	// counter is the only trace they leave, so a run's edge count is never
	// silently inflated without visibility into why.
	SkippedInStringLiteral int
}

// Options bounds a walk. Zero-value fields fall back to the Default* constants.
type Options struct {
	MaxDepth     int
	MaxNodes     int
	MaxFileBytes int64
}

// Default bounds. These are deliberately conservative: this package parses
// operator-controlled text and must degrade to ReasonDepthLimit rather than
// unbounded recursion or memory growth on an adversarial or pathological tree.
const (
	DefaultMaxDepth     = 64
	DefaultMaxNodes     = 20000
	DefaultMaxFileBytes = 4 << 20 // 4 MiB
)

// DefaultOptions returns the recommended bounds for a typical project tree.
func DefaultOptions() Options {
	return Options{MaxDepth: DefaultMaxDepth, MaxNodes: DefaultMaxNodes, MaxFileBytes: DefaultMaxFileBytes}
}

func (o Options) normalized() Options {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = DefaultMaxNodes
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	return o
}

// Walk statically resolves the CMake include()/add_subdirectory() graph
// starting at startFile. workspaceRoot is REQUIRED (not optional): it is both
// the hard boundary no resolved path may escape (ReasonOutsideWorkspace
// otherwise) and the value substituted for ${CMAKE_SOURCE_DIR}. Read-only:
// never writes, never executes cmake, never shells out.
func Walk(startFile, workspaceRoot string, opts Options) (*Result, error) {
	if strings.TrimSpace(startFile) == "" {
		return nil, errors.New("cmakegraph: startFile is required")
	}
	w, err := newWalker(workspaceRoot, opts)
	if err != nil {
		return nil, err
	}
	absStart, err := w.walkRoot(startFile)
	if err != nil {
		return nil, err
	}
	return w.result(absStart), nil
}

// WalkTree finds EVERY file under root matching one of entryNames and walks
// each as an independent root, sharing ONE traversal state across all of
// them — so a file is scanned at most once regardless of how many roots
// would reach it, which is what guarantees the merged Result never contains
// a duplicate edge (rather than merging N independent walks and de-duplicating
// after the fact). workspaceRoot for every constituent walk is root itself.
//
// entryNames entries are either an exact basename (case-insensitive, e.g.
// "CMakeLists.txt") or an extension pattern "*.ext" (case-insensitive, e.g.
// "*.cmake"). Passing []string{"*.cmake", "CMakeLists.txt"} scans the WHOLE
// tree's CMake-processable files as independent roots — the right choice for
// a tree with no single top-level CMakeLists.txt (e.g. a vcpkg-style
// overlay-ports tree, where the real graph lives across portfile.cmake AND
// vcpkg's own toolchain/port-tweak *.cmake helper files, most of which no
// portfile.cmake ever reaches directly). Passing []string{"portfile.cmake"}
// reproduces the earlier, narrower "one root per port" behavior.
//
// Depth (MaxDepth) is bounded PER ROOT, same as a single Walk. MaxNodes is a
// WHOLE-TREE bound shared across every root in this call (len of the shared
// visited set), since the point of a total-node cap is bounding the overall
// scan cost, not each root's slice of it.
func WalkTree(root string, entryNames []string, opts Options) (*Result, error) {
	if len(entryNames) == 0 {
		return nil, errors.New("cmakegraph: entryNames must be non-empty")
	}
	w, err := newWalker(root, opts)
	if err != nil {
		return nil, err
	}
	var starts []string
	walkErr := filepath.WalkDir(w.workspaceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Best-effort: an unreadable entry is skipped, not fatal.
			return nil
		}
		if !d.IsDir() && matchesEntry(d.Name(), entryNames) {
			starts = append(starts, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("cmakegraph: enumerate entry files under %s: %w", w.workspaceRoot, walkErr)
	}
	sort.Strings(starts) // deterministic root order, so a shared file's edge always attributes to the same first-seen context

	for _, s := range starts {
		if _, err := w.walkRoot(s); err != nil {
			return nil, fmt.Errorf("cmakegraph: walk %s: %w", s, err)
		}
	}
	return w.result(w.workspaceRoot), nil
}

// matchesEntry reports whether name (a bare file basename) matches one of
// patterns. A pattern of the form "*.ext" matches by extension
// (case-insensitive); any other pattern matches the exact basename
// (case-insensitive).
func matchesEntry(name string, patterns []string) bool {
	lower := strings.ToLower(name)
	for _, p := range patterns {
		lp := strings.ToLower(p)
		if strings.HasPrefix(lp, "*.") {
			if strings.HasSuffix(lower, lp[1:]) {
				return true
			}
			continue
		}
		if lower == lp {
			return true
		}
	}
	return false
}

// walker carries traversal state SHARED across every root walked in one Walk
// or WalkTree call — this is what guarantees a file is scanned at most once
// (and so its edges appear at most once) no matter how many roots reach it.
type walker struct {
	opts              Options
	workspaceRoot     string // absolute, NOT symlink-resolved (used for ${CMAKE_SOURCE_DIR} substitution)
	realWorkspaceRoot string // symlink-resolved, used for the boundary check
	visited           map[string]bool
	files             map[string]bool
	edges             []Edge
	skippedInString   int
}

// newWalker validates workspaceRoot and constructs empty shared traversal state.
func newWalker(workspaceRoot string, opts Options) (*walker, error) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil, errors.New("cmakegraph: workspaceRoot is required (it is the hard outside_workspace boundary and the ${CMAKE_SOURCE_DIR} value)")
	}
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("cmakegraph: resolve workspace root: %w", err)
	}
	realRoot := absRoot
	if resolved, evalErr := filepath.EvalSymlinks(absRoot); evalErr == nil {
		realRoot = resolved
	}
	return &walker{
		opts:              opts.normalized(),
		workspaceRoot:     absRoot,
		realWorkspaceRoot: realRoot,
		visited:           map[string]bool{},
		files:             map[string]bool{},
	}, nil
}

// walkRoot validates startFile and scans it as a fresh traversal root — UNLESS
// it was already visited (as an earlier root, or reached via traversal from
// one), in which case it is a no-op: its edges were already discovered.
// Returns the absolute path of startFile.
func (w *walker) walkRoot(startFile string) (string, error) {
	absStart, err := filepath.Abs(startFile)
	if err != nil {
		return "", fmt.Errorf("cmakegraph: resolve start file: %w", err)
	}
	if fi, statErr := os.Stat(absStart); statErr != nil || fi.IsDir() {
		if statErr != nil {
			return "", fmt.Errorf("cmakegraph: start file %s: %w", absStart, statErr)
		}
		return "", fmt.Errorf("cmakegraph: start file %s is a directory, want a file", absStart)
	}
	canonStart := w.canonicalize(absStart)
	if w.visited[canonStart] {
		return absStart, nil
	}
	w.visited[canonStart] = true
	w.files[canonStart] = true
	w.walkFile(absStart, filepath.Dir(canonStart), 0, []string{canonStart})
	return absStart, nil
}

func (w *walker) result(root string) *Result {
	sortedFiles := make([]string, 0, len(w.files))
	for f := range w.files {
		sortedFiles = append(sortedFiles, f)
	}
	sort.Strings(sortedFiles)

	hist := Histogram{ByReason: map[Reason]int{}}
	for _, e := range w.edges {
		switch e.Status {
		case StatusResolved:
			hist.Resolved++
		case StatusDangling:
			hist.Dangling++
		case StatusUnresolved:
			hist.Unresolved++
			hist.ByReason[e.Reason]++
		}
	}
	return &Result{
		Root:                   root,
		WorkspaceRoot:          w.workspaceRoot,
		Edges:                  w.edges,
		Files:                  sortedFiles,
		Histogram:              hist,
		SkippedInStringLiteral: w.skippedInString,
	}
}

// walkFile scans one file's content for include()/add_subdirectory() calls
// and recurses into newly-resolved, in-budget, non-cyclic targets.
// sourceDir is this package's internal (never textually substituted) tracked
// approximation of CMAKE_CURRENT_SOURCE_DIR, used ONLY to resolve bare
// (variable-free) add_subdirectory() targets. ancestors is the current DFS
// path (canonical file paths) used for true-cycle detection — distinct from
// the whole-run visited set, so a diamond re-include is never misreported as
// a cycle.
func (w *walker) walkFile(file, sourceDir string, depth int, ancestors []string) {
	data, err := readBounded(file, w.opts.MaxFileBytes)
	if err != nil {
		// Cannot scan further. The edge that led here already recorded its
		// own Resolved/Dangling verdict from filesystem facts alone; we just
		// stop expanding the graph from this node (see package doc).
		return
	}
	clean, quoted := preprocess(data)
	canonSelf := w.canonicalize(file)
	listDir := filepath.Dir(canonSelf)

	calls, skipped := extractCalls(clean, quoted)
	w.skippedInString += skipped

	for _, c := range calls {
		line := lineOf(clean, c.Offset)
		conditional := conditionalDepthBefore(clean, quoted, c.Offset) > 0
		e := Edge{Kind: c.Kind, FromFile: canonSelf, Line: line, RawArg: c.RawArg, Conditional: conditional}

		candidate, reason, ok := classifyArg(c.Kind, c.RawArg, c.Malformed, listDir, sourceDir, w.workspaceRoot)
		if !ok {
			e.Status = StatusUnresolved
			e.Reason = reason
			w.edges = append(w.edges, e)
			continue
		}

		if !w.withinWorkspace(candidate) {
			e.Status = StatusUnresolved
			e.Reason = ReasonOutsideWorkspace
			e.ResolvedPath = candidate
			w.edges = append(w.edges, e)
			continue
		}
		e.ResolvedPath = candidate

		var targetFile, newSourceDir string
		var exists bool
		switch c.Kind {
		case EdgeInclude:
			if fi, statErr := os.Stat(candidate); statErr == nil && !fi.IsDir() {
				exists = true
				targetFile = candidate
			}
			newSourceDir = sourceDir
		case EdgeAddSubdirectory:
			newSourceDir = candidate
			if fi, statErr := os.Stat(candidate); statErr == nil && fi.IsDir() {
				cml := filepath.Join(candidate, "CMakeLists.txt")
				if cfi, cerr := os.Stat(cml); cerr == nil && !cfi.IsDir() {
					exists = true
					targetFile = cml
				}
			}
		}

		if !exists {
			e.Status = StatusDangling
			w.edges = append(w.edges, e)
			continue
		}

		canonTarget := w.canonicalize(targetFile)
		switch {
		case containsStr(ancestors, canonTarget):
			e.Status = StatusUnresolved
			e.Reason = ReasonCyclic
		case w.visited[canonTarget]:
			// Already scanned — either a diamond re-include from a sibling
			// path, or this exact file was itself (or will be) walked as its
			// own independent WalkTree root. Not a cycle; not re-scanned
			// (its edges were already discovered exactly once).
			e.Status = StatusResolved
		case depth+1 > w.opts.MaxDepth || len(w.visited) >= w.opts.MaxNodes:
			e.Status = StatusUnresolved
			e.Reason = ReasonDepthLimit
		default:
			e.Status = StatusResolved
			w.visited[canonTarget] = true
			w.files[canonTarget] = true
			w.edges = append(w.edges, e)
			nextAncestors := make([]string, len(ancestors)+1)
			copy(nextAncestors, ancestors)
			nextAncestors[len(ancestors)] = canonTarget
			w.walkFile(targetFile, newSourceDir, depth+1, nextAncestors)
			continue
		}
		w.edges = append(w.edges, e)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (w *walker) canonicalize(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return filepath.Clean(abs)
}

// withinWorkspace reports whether candidate (which may not exist) resolves,
// after following symlinks on its nearest existing ancestor, to a path inside
// w.realWorkspaceRoot.
func (w *walker) withinWorkspace(candidate string) bool {
	real := realOrNearestAncestor(candidate)
	rel, err := filepath.Rel(w.realWorkspaceRoot, real)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// realOrNearestAncestor walks up from p until it finds an existing ancestor,
// resolves THAT ancestor's symlinks, then re-appends the (non-existent) tail.
func realOrNearestAncestor(p string) string {
	cur := p
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if cur == p {
				return real
			}
			rest, relErr := filepath.Rel(cur, p)
			if relErr != nil {
				return real
			}
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		cur = parent
	}
}

func readBounded(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("cmakegraph: %s exceeds byte cap (%d > %d)", path, info.Size(), maxBytes)
	}
	return io.ReadAll(f)
}

// preprocess scans data ONCE, returning (a) a comment-stripped copy safe for
// regex scanning — `# ... end-of-line` comments outside double quotes are
// blanked to spaces, preserving byte length and newline positions so
// offset/line math against the original file stays valid — and (b) a
// parallel boolean mask, quoted[i], true iff byte i of the RETURNED clean
// slice sits inside a double-quoted span in the source. Both share the same
// quote-toggle state machine (an unescaped `"` toggles; `\"` does not).
func preprocess(data []byte) (clean []byte, quoted []bool) {
	clean = make([]byte, len(data))
	copy(clean, data)
	quoted = make([]bool, len(data))
	inQuote := false
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		switch {
		case c == '"' && (i == 0 || clean[i-1] != '\\'):
			inQuote = !inQuote
			quoted[i] = inQuote
		case c == '#' && !inQuote:
			for i < len(clean) && clean[i] != '\n' {
				clean[i] = ' '
				i++
			}
		default:
			quoted[i] = inQuote
		}
	}
	return clean, quoted
}

var commandRe = regexp.MustCompile(`(?i)\b(include|add_subdirectory)\s*\(`)
var condRe = regexp.MustCompile(`(?i)\b(if|endif)\s*\(`)

// rawCall is one lexically-discovered include()/add_subdirectory() call site.
type rawCall struct {
	Kind      EdgeKind
	Offset    int // offset of the command-name start, used for line numbers
	RawArg    string
	Malformed bool // true when the call syntax could not be safely parsed
}

// extractCalls finds every include()/add_subdirectory() call in clean
// (comment-stripped) content and extracts each one's first argument token. A
// match whose opening `(` falls inside a double-quoted span per quoted (a
// command name embedded in an unrelated string-literal argument, e.g.
// vcpkg_replace_string(...)'s find/replace text — see the package doc's D3
// note) is skipped entirely: no rawCall is produced for it, and skipped is
// incremented instead.
func extractCalls(clean []byte, quoted []bool) (calls []rawCall, skipped int) {
	for _, loc := range commandRe.FindAllSubmatchIndex(clean, -1) {
		nameStart, nameEnd := loc[2], loc[3]
		name := strings.ToLower(string(clean[nameStart:nameEnd]))
		var kind EdgeKind
		switch name {
		case "include":
			kind = EdgeInclude
		case "add_subdirectory":
			kind = EdgeAddSubdirectory
		default:
			continue
		}
		openParen := loc[1] - 1 // loc[1] is the match end, right after '('
		if openParen >= 0 && openParen < len(quoted) && quoted[openParen] {
			skipped++
			continue
		}
		closeIdx, ok := findMatchingParen(clean, openParen)
		if !ok {
			calls = append(calls, rawCall{Kind: kind, Offset: loc[0], Malformed: true})
			continue
		}
		argText := string(clean[loc[1]:closeIdx])
		tok, tokOK := firstToken(argText)
		calls = append(calls, rawCall{Kind: kind, Offset: loc[0], RawArg: tok, Malformed: !tokOK})
	}
	return calls, skipped
}

// findMatchingParen returns the index of the ')' matching the '(' at
// data[openIdx], skipping parens inside double-quoted spans. ok is false if
// no matching close was found before EOF.
func findMatchingParen(data []byte, openIdx int) (int, bool) {
	depth := 0
	inQuote := false
	for i := openIdx; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '"' && (i == 0 || data[i-1] != '\\'):
			inQuote = !inQuote
		case inQuote:
			// inside a quoted string; parens here don't count
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return -1, false
}

// firstToken returns the first whitespace- or quote-delimited token in s,
// stripping surrounding quotes. ok is false for an empty/whitespace-only
// argument list or an unterminated quote.
func firstToken(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t\r\n")
	if s == "" {
		return "", false
	}
	if s[0] == '"' {
		for i := 1; i < len(s); i++ {
			if s[i] == '"' && s[i-1] != '\\' {
				return s[1:i], true
			}
		}
		return "", false
	}
	if end := strings.IndexAny(s, " \t\r\n"); end >= 0 {
		return s[:end], true
	}
	return s, true
}

// conditionalDepthBefore returns the if()/endif() nesting depth immediately
// before offset, using a simple per-file if()=+1 / endif()=-1 counter. A
// match whose opening `(` is inside a quoted string (per quoted — same guard
// as extractCalls) is ignored, so an if(/endif( appearing inside an
// unrelated string literal cannot corrupt the count. This does not model
// elseif()/else() branch selection or function()/macro() bodies — see the
// package doc's "Known, documented simplifications".
func conditionalDepthBefore(clean []byte, quoted []bool, offset int) int {
	depth := 0
	for _, loc := range condRe.FindAllSubmatchIndex(clean, -1) {
		if loc[0] >= offset {
			break
		}
		openParen := loc[1] - 1
		if openParen >= 0 && openParen < len(quoted) && quoted[openParen] {
			continue
		}
		name := strings.ToLower(string(clean[loc[2]:loc[3]]))
		if name == "if" {
			depth++
		} else {
			depth--
			if depth < 0 {
				depth = 0
			}
		}
	}
	return depth
}

func lineOf(data []byte, offset int) int {
	if offset > len(data) {
		offset = len(data)
	}
	line := 1
	for _, b := range data[:offset] {
		if b == '\n' {
			line++
		}
	}
	return line
}

// classifyArg applies the resolution precedence documented at the package
// level and returns either a concrete candidate path (ok=true) or an
// unresolved reason (ok=false, candidate=""). Whether the call is inside a
// conditional block is NOT a resolution input here — it is recorded on the
// Edge separately (Edge.Conditional) and never blocks path computation; see
// the package doc's "Conditional calls" section.
func classifyArg(kind EdgeKind, rawArg string, malformed bool, listDir, sourceDir, workspaceRoot string) (candidate string, reason Reason, ok bool) {
	if malformed || strings.TrimSpace(rawArg) == "" {
		return "", ReasonParseError, false
	}
	if strings.Contains(rawArg, "$<") {
		return "", ReasonGeneratorExpression, false
	}
	if strings.Contains(rawArg, "$ENV{") {
		return "", ReasonNonStaticVariable, false
	}

	hadVariable := strings.Contains(rawArg, "${")
	substituted, allKnown := substituteKnownVars(rawArg, listDir, workspaceRoot)
	if !allKnown {
		return "", ReasonNonStaticVariable, false
	}

	if kind == EdgeInclude && !hadVariable && looksLikeModuleName(substituted) {
		return "", ReasonModuleNameNotPath, false
	}

	base := listDir
	if kind == EdgeAddSubdirectory {
		base = sourceDir
	}
	p := substituted
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), "", true
}

// looksLikeModuleName reports whether s (after substitution, with no
// variables left) looks like a bare CMake module name rather than a path:
// no path separator and no .cmake suffix.
func looksLikeModuleName(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	return !strings.HasSuffix(strings.ToLower(s), ".cmake")
}

// substituteKnownVars replaces ${CMAKE_CURRENT_LIST_DIR} and ${CMAKE_SOURCE_DIR}
// references in s. ok is false if s contains any OTHER ${...} reference
// (including the deliberately-unresolved ${CMAKE_CURRENT_SOURCE_DIR}) or an
// unterminated ${.
func substituteKnownVars(s, listDir, workspaceRoot string) (string, bool) {
	var b strings.Builder
	i := 0
	for {
		idx := strings.Index(s[i:], "${")
		if idx < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+idx])
		start := i + idx + 2
		end := strings.IndexByte(s[start:], '}')
		if end < 0 {
			return "", false
		}
		name := s[start : start+end]
		switch name {
		case "CMAKE_CURRENT_LIST_DIR":
			b.WriteString(listDir)
		case "CMAKE_SOURCE_DIR":
			b.WriteString(workspaceRoot)
		default:
			return "", false
		}
		i = start + end + 1
	}
	return b.String(), true
}
