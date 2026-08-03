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
// # FAIL CLOSED is the governing principle
//
// Where the true CMake context is not provable from static text, the answer
// is StatusUnresolved with a Reason — NEVER StatusResolved. A cross-family
// adversarial review found that an earlier version of this package satisfied
// the letter of "Resolved means os.Stat succeeded" while violating the
// spirit of it: the CANDIDATE PATH or the CALL SITE ITSELF could be wrong
// (resolved against the wrong base directory, or lexically inside a
// macro()/function() body where the substituted variable's true value is
// invocation-dependent), so a file existing at the computed path did not
// mean it was the file CMake would actually use. Under this package's
// contract that is a false Resolved — the one failure mode that makes the
// tool worse than useless. Every fix below narrows Resolved rather than
// widening it: expect the resolved fraction of any real tree to be smaller
// than a naive lexical scan would suggest. A smaller true number beats a
// larger unverified one.
//
// # Tri-state resolution, never a false positive
//
// Every include()/add_subdirectory() call site becomes one Edge with one of
// three Status values:
//
//   - StatusResolved: a concrete path was computed AND os.Stat confirms the
//     target exists (a file for include(), or a directory containing its own
//     CMakeLists.txt for add_subdirectory()) AND the computation itself
//     rested only on premises this package can verify (see the sections
//     below) — never on an invented or invocation-dependent guess.
//   - StatusDangling: a concrete, verified path was computed and the target
//     is VERIFIABLY absent — os.Stat returned fs.ErrNotExist, or the target
//     exists as the wrong TYPE (a directory where include() needs a file, a
//     non-directory where add_subdirectory() needs one). This is a real
//     finding (a broken include), not a scanner failure. An os.Stat that
//     fails for ANY OTHER reason (access denied, sharing violation,
//     transient I/O) proves nothing about absence and is
//     StatusUnresolved/ReasonTargetUnreadable instead — see that constant.
//   - StatusUnresolved: the scanner could not safely compute a verified path
//     at all. Reason names exactly why (see the Reason constants) — this is
//     the scanner declining to guess rather than reporting a wrong
//     "resolved".
//
// # Conditional calls: the path is resolved anyway
//
// A call textually nested inside an if()/elseif()/else(), foreach(), or
// while() block answers two INDEPENDENT questions: "where does this point?" (fully statically
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
// # Relative include()/add_subdirectory() arguments resolve against
// CMAKE_CURRENT_SOURCE_DIR, NOT CMAKE_CURRENT_LIST_DIR
//
// CMake's own include() implementation resolves a relative argument against
// the CURRENT SOURCE DIRECTORY (the add_subdirectory()-established directory
// scope), not the calling list file's own directory. These two are IDENTICAL
// at the top of a file that is not itself include()-d, but DIVERGE precisely
// inside an included file: CMAKE_CURRENT_LIST_DIR becomes the included
// file's own directory, while CMAKE_CURRENT_SOURCE_DIR stays whatever the
// enclosing directory scope already was (include() never changes it — only
// add_subdirectory() does). This package tracks CMAKE_CURRENT_SOURCE_DIR
// internally (see sourceContext below) and uses it as the base for BOTH
// include() and add_subdirectory() bare-relative arguments. An argument that
// explicitly writes ${CMAKE_CURRENT_LIST_DIR} still resolves against the
// calling file's own directory — that is literally what the variable means,
// unconditionally, and is exactly why real CMake code (including this
// package's own primary field-tested source, a vcpkg-style overlay-ports
// tree) idiomatically writes ${CMAKE_CURRENT_LIST_DIR} explicitly rather than
// relying on the bare-relative form.
//
// # Source-dir context must be VERIFIED, never invented
//
// A CMakeLists.txt file reached as a Walk/WalkTree root, or as an
// add_subdirectory() target, has a well-founded CMAKE_CURRENT_SOURCE_DIR:
// its own directory. An arbitrary *.cmake file scanned as an independent
// WalkTree discovery root (e.g. under a "*.cmake" entryNames pattern) does
// NOT — such a file could be include()-d from anywhere in the real project,
// under any CMAKE_CURRENT_SOURCE_DIR, and this package has no way to know
// which. sourceContext.verified tracks this distinction; a bare-relative
// argument resolved against an UNVERIFIED context is refused
// (StatusUnresolved / ReasonUnverifiedSourceDir) rather than guessed from
// "the file's own directory". Verification propagates correctly through
// traversal: include() never changes verified-ness (it doesn't change the
// source dir at all); a successfully-resolved add_subdirectory() target is
// ALWAYS verified going forward (reaching that point means the candidate was
// computed either from an absolute variable or from an already-verified
// context, so the child's own directory is now concretely known regardless
// of how uncertain the ORIGINAL root was).
//
// Traversal state (the visited/dedup set) is keyed by (file, sourceContext),
// not file path alone — a file legitimately reachable under two DIFFERENT
// verified contexts is explored under both, rather than the second visit
// being silently suppressed by the first (which would erase a context this
// package could otherwise have verified). Result.Files still lists each
// physical file once (it answers "what did we open", not "under how many
// contexts"); Edge.FromFile can therefore appear more than once across
// genuinely different contexts, each independently resolved.
//
// ${CMAKE_CURRENT_LIST_DIR} is NOT purely lexical either: CMake documents it
// as reflecting the BOTTOM-MOST INVOKING file when the reference sits inside
// a macro()/function() body, not the file that defines the macro/function.
// This package detects that a call is lexically inside an open
// macro()/function()...endmacro()/endfunction() block (a simple per-file
// nesting counter, like the if()/endif() one) and refuses any
// ${CMAKE_CURRENT_LIST_DIR} substitution there — StatusUnresolved /
// ReasonDeferredMacroContext — rather than substituting the DEFINING file's
// own directory, which would silently assume the macro is invoked from
// itself. This does not model macro/function INVOCATION at all (v1 scope);
// it only detects the DEFINITION-site nesting.
//
// # Unsupported argument syntax fails closed, never stats raw text
//
// Beyond the already-refused ${...} variables, $ENV{...}, and $<...>
// generator expressions: $CACHE{...} cache-variable references are refused
// the same way (ReasonNonStaticVariable); CMake bracket-argument syntax
// (`[[...]]`, `[=[...]=]`) is recognized structurally by the lexer (see
// below) but its content is never extracted as a path (ReasonParseError —
// this package declines to support the form, rather than guessing at
// mis-parsed content); and any argument containing a raw backslash is
// refused outright (ReasonParseError) rather than attempting to decode
// CMake's escape-sequence rules — a decode bug on a Windows-style path is
// exactly the kind of false Resolved (or worse, a wrong absolute path
// silently stat'd) this package's contract forbids.
//
// # The lexer: one structural pass, not regex + a bolted-on quote mask
//
// Call sites are discovered by scanTopLevelCommands, a single forward pass
// over the RAW file bytes that understands: line comments (# to end of
// line), bracket comments (#[[ ... ]], #[=[ ... ]=], ... — matched to their
// ACTUAL closing bracket, not just stripped on the opening line), quoted
// arguments with correct escape PARITY (a run of N backslashes immediately
// before a quote character closes the string iff N is even — not "look at
// only the one preceding byte", which mis-parses message("x\\") and can
// swallow the next real command), bracket arguments (recognized as opaque,
// not decoded — and, critically, distinguishing "this '[' is not a bracket
// opener at all" from "this IS a valid opener with no matching close": the
// second is a syntax breakdown, because treating it as a literal '[' would
// re-expose the payload's own bytes as executable syntax, letting a ')'
// inside an unterminated `[=[` close the enclosing call so that a following
// include() is read as a REAL edge), and — the key structural property — COMMAND NESTING: once
// the scanner is inside one command's argument-list span (from its `(` to
// the matching `)`, correctly skipping over quoted/bracket spans and nested
// balanced parens), it never independently re-recognizes a command-name-
// shaped substring WITHIN that span as a fresh top-level call. This is what
// makes message(include(existing.cmake)) — a single, syntactically legal
// unquoted argument to message(), NOT an include() directive — produce no
// edge, and what makes message(if(fake)) unable to corrupt the
// if()/endif()/macro()/function() nesting counters used for Edge.Conditional
// and the deferred-macro-context gate: those counters are derived from the
// SAME top-level command list, so a command name nested inside another
// call's argument list was never a candidate to affect them in the first
// place.
//
// An unbalanced/unterminated command call (missing closing paren before EOF)
// halts scanning of the REST of that file — no resync heuristic is
// attempted after a genuine syntax breakdown; commands recognized BEFORE the
// break point are unaffected, and other files are scanned independently.
//
// # Every coverage hole is REPORTED; bounds are enforced before allocation
//
// The one thing this package must never do is hand back a partial graph that
// LOOKS complete. Four distinct bounds and failures can stop it short, and
// each has its own visible signal:
//
//   - MaxRoots bounds WalkTree's candidate-root ENUMERATION, so a tree with
//     millions of matching files cannot grow the enumeration slice without
//     bound before a single node is admitted (Result.RootEnumerationCapped).
//   - MaxNodes gates root ADMISSION, not just edge traversal within an
//     already-admitted root (Result.NodeCapTruncated,
//     Result.RootsSkippedByNodeCap).
//   - A subtree that cannot be ENUMERATED (an ACL-denied directory) is
//     recorded as a CoverageEnumerateFailed hole and enumeration continues;
//     a failure to enumerate the ROOT ITSELF is fatal and returns an error,
//     because "no matching files here" and "could not look" are different
//     answers and only one of them is evidence.
//   - A file that could not be READ (byte-cap trip, permission error, TOCTOU
//     race after a successful Stat) is recorded as a coverage hole with its
//     closed CoverageReason — the edge that led to it still carries a genuine
//     StatusResolved (the filesystem fact "this file exists" remains true),
//     but its own outgoing edges are known-missing, not silently absent.
//
// Every one of those holes goes through ONE recorder (walker.recordCoverage)
// into Result.UnscannedFiles, so a caller has a single place to look and no
// future code path can drop a hole by forgetting to append.
//
// # The workspace boundary is enforced before the first read
//
// An enumerated root is canonicalized (symlinks resolved) and checked against
// the workspace boundary BEFORE os.Stat or any read: a matching *.cmake entry
// inside the tree may be a symlink pointing outside it, and opening it to
// find that out would already have performed the read the boundary exists to
// prevent. Such a root is refused and recorded, never scanned.
//
// # Cancellation
//
// Walk/WalkTree take a context and check it between roots, between files, and
// between commands within a file. A canceled or timed-out walk RETURNS the
// error rather than handing back the partial graph it had accumulated —
// truncation is only ever reported through the coverage signals above, and a
// caller must never have to guess which of the two it received.
//
// # Known, documented simplifications
//
//   - Argument parsing takes only the FIRST whitespace/quote-delimited token
//     of an include()/add_subdirectory() call as the path/module argument,
//     ignoring trailing keyword arguments (OPTIONAL, RESULT_VARIABLE,
//     EXCLUDE_FROM_ALL, add_subdirectory's optional binary-dir argument).
//     This matches the dominant real-world call shape.
//   - Conditional and deferred-macro-context detection are simple per-file
//     lexical frame stacks; they do not model if()/elseif()/else() branch
//     selection or loop execution (any call nested inside an open
//     if()/foreach()/while() frame is flagged conditional), are NOT transitive
//     across file boundaries (a call is flagged only by a control frame or
//     macro()/function() in ITS OWN file — if that file was itself reached via
//     a conditional or deferred-macro edge, this package does not propagate
//     that fact onto the file's own calls), and do not model macro/function
//     INVOCATION (only lexical DEFINITION-site nesting).
//   - CMAKE_MODULE_PATH is never consulted: a bare include() argument with no
//     path separator and no .cmake suffix is refused (ReasonModuleNameNotPath)
//     rather than guessed, regardless of what CMAKE_MODULE_PATH might
//     actually contain for a real project.
//   - The (file, sourceContext)-keyed traversal means a file reachable under
//     multiple DIFFERENT verified contexts is fully re-explored under each —
//     more thorough than the earlier canonical-path-only dedup, at the cost
//     of potentially redundant re-scanning bounded by MaxNodes.
package cmakegraph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Status is the tri-state resolution outcome of one include()/add_subdirectory() edge.
type Status int

const (
	// StatusUnknown is the zero value and must never appear in a returned Edge.
	StatusUnknown Status = iota
	// StatusResolved means a concrete, VERIFIED path was computed AND confirmed to exist.
	StatusResolved
	// StatusDangling means a concrete, VERIFIED path was computed but the target is absent.
	StatusDangling
	// StatusUnresolved means no concrete, safely-verified path could be computed; see Reason.
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
	// $ENV{...}, or $CACHE{...}) this package does not know is safe to
	// substitute — including the deliberately-unresolved
	// ${CMAKE_CURRENT_SOURCE_DIR}.
	ReasonNonStaticVariable Reason = "non_static_variable"
	// ReasonGeneratorExpression: the argument contains a $<...> generator
	// expression, which is only meaningful at CMake generate time.
	ReasonGeneratorExpression Reason = "generator_expression"
	// ReasonModuleNameNotPath: an include() argument with no path separator
	// and no .cmake suffix — a bare CMake module name resolved via
	// CMAKE_MODULE_PATH, which this package does not have access to.
	ReasonModuleNameNotPath Reason = "module_name_not_path"
	// ReasonParseError: the call's own argument syntax could not be safely
	// extracted as a path — an empty argument list, an unterminated quote,
	// CMake bracket-argument syntax (recognized but not decoded), or an
	// argument containing a raw backslash (escape sequences are refused,
	// never decoded — see the package doc).
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
	// ReasonDeferredMacroContext: the argument references
	// ${CMAKE_CURRENT_LIST_DIR} from a call site lexically inside an open
	// macro()/function() body, where the variable's true value depends on
	// the invoking file at expansion time, not the defining file.
	ReasonDeferredMacroContext Reason = "deferred_macro_context"
	// ReasonUnverifiedSourceDir: a relative argument would need to resolve
	// against CMAKE_CURRENT_SOURCE_DIR, but the current traversal context's
	// source directory was never verified (e.g. an arbitrary *.cmake file
	// scanned as an independent discovery root) — this package refuses to
	// invent one.
	ReasonUnverifiedSourceDir Reason = "unverified_source_dir"
	// ReasonTargetUnreadable: a concrete, verified path was computed but the
	// filesystem could not tell us whether it EXISTS — os.Stat failed with
	// something other than fs.ErrNotExist (access denied, a sharing
	// violation, a transient I/O error, a too-long path). StatusDangling
	// means VERIFIED ABSENCE and must never be used for that: an unreadable
	// target is an unknown target, so it fails closed here instead.
	ReasonTargetUnreadable Reason = "target_unreadable"
)

// CoverageReason is the CLOSED set of ways this package can fail to cover
// part of the tree it was asked about. Every value here means "we know we
// did NOT look at this", never "there was nothing here" — the whole point of
// Result.UnscannedFiles is that a caller can tell those two apart.
type CoverageReason string

const (
	// CoverageByteCapExceeded: the file is larger than Options.MaxFileBytes,
	// so its content — and therefore its own outgoing edges — was never read.
	CoverageByteCapExceeded CoverageReason = "byte_cap_exceeded"
	// CoverageFileUnreadable: opening or reading the file failed (permission
	// error, a TOCTOU race after a successful Stat, an I/O error).
	CoverageFileUnreadable CoverageReason = "file_unreadable"
	// CoverageEnumerateFailed: a directory subtree under WalkTree's root
	// could not be enumerated (typically an ACL-denied directory). Any
	// matching CMake file inside it was never discovered, so the returned
	// graph is INCOMPLETE — it is not evidence that the subtree held nothing.
	CoverageEnumerateFailed CoverageReason = "enumerate_failed"
	// CoverageRootOutsideWorkspace: an enumerated root canonicalized (after
	// symlink resolution) to a path outside the workspace boundary and was
	// refused BEFORE any Stat or read. The boundary is the contract; a
	// symlinked entry inside the tree does not get to escape it.
	CoverageRootOutsideWorkspace CoverageReason = "root_outside_workspace"
	// CoverageRootEnumerationCapped: Options.MaxRoots was reached while
	// enumerating candidate roots, so enumeration stopped early. Path names
	// the directory the walk was in when the cap tripped.
	CoverageRootEnumerationCapped CoverageReason = "root_enumeration_capped"
	// CoverageSymlinkDirectorySkipped: a directory symlink was encountered
	// while enumerating WalkTree. Its subtree was deliberately not traversed,
	// so any matching CMake files behind it are absent from the result.
	CoverageSymlinkDirectorySkipped CoverageReason = "symlink_directory_skipped"
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
	Kind EdgeKind
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
	// if()/elseif()/else(), foreach()/endforeach(), or while()/endwhile()
	// control frame in ITS OWN file (see the package doc's "Conditional
	// calls" section). It is independent of Status: a conditional edge is
	// Resolved/Dangling by the exact same os.Stat rule as an unconditional one.
	// Conditional == true means only "whether this executes at configure time
	// is unknown", never "the path is unknown".
	Conditional bool
}

// Histogram tallies edges by outcome. This is the operator-facing go/no-go metric.
type Histogram struct {
	Resolved   int
	Dangling   int
	Unresolved int
	ByReason   map[Reason]int
}

// UnscannedFile records one COVERAGE HOLE: a file or subtree this package
// knows it did not look inside. Whatever is behind it (edges, further files)
// is known-missing from the Result, never silently absent. Path is
// best-effort (absolute, symlink-resolved when possible).
//
// Reason is a CLOSED enum (see CoverageReason) so a caller can switch on it;
// Detail carries the underlying error text for a human and is diagnostic
// only — never parse it, never branch on it.
type UnscannedFile struct {
	Path   string
	Reason CoverageReason
	Detail string
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
	// (deduplicated, sorted, absolute+symlink-resolved paths) — one entry
	// per PHYSICAL file regardless of how many distinct source contexts it
	// was explored under.
	Files     []string
	Histogram Histogram
	// NodeCapTruncated is true iff at least one independent WalkTree root was
	// refused admission because the shared MaxNodes budget was already
	// exhausted (as opposed to a within-traversal ReasonDepthLimit edge).
	NodeCapTruncated bool
	// RootsSkippedByNodeCap counts how many independent roots were refused
	// admission for that reason.
	RootsSkippedByNodeCap int
	// RootEnumerationCapped is true iff WalkTree stopped ENUMERATING candidate
	// roots because Options.MaxRoots was reached. Distinct from
	// NodeCapTruncated: that one means "enumerated but not admitted", this one
	// means "never even enumerated", so the number skipped is unknowable by
	// construction. Either way the graph is incomplete.
	RootEnumerationCapped bool
	// UnscannedFiles lists every COVERAGE HOLE — a file whose content could
	// not be read, a subtree that could not be enumerated or was skipped for a
	// directory symlink, a root refused for escaping the workspace, and the
	// point enumeration was capped at. See
	// UnscannedFile's doc.
	UnscannedFiles []UnscannedFile
}

// Options bounds a walk. Zero-value fields fall back to the Default* constants.
type Options struct {
	MaxDepth     int
	MaxNodes     int
	MaxFileBytes int64
	// MaxRoots bounds how many candidate root paths WalkTree will ENUMERATE
	// (and therefore hold in memory) before it stops walking the tree. It is
	// deliberately separate from MaxNodes: enumeration happens before, and is
	// far cheaper per item than, admission, so collapsing the two would make
	// the admission gate unreachable whenever every root is a leaf.
	MaxRoots int
}

// Default bounds. These are deliberately conservative: this package parses
// operator-controlled text and must degrade to ReasonDepthLimit rather than
// unbounded recursion or memory growth on an adversarial or pathological tree.
const (
	DefaultMaxDepth     = 64
	DefaultMaxNodes     = 20000
	DefaultMaxFileBytes = 4 << 20 // 4 MiB
	// DefaultMaxRoots bounds WalkTree's candidate-root enumeration. Sized
	// well above DefaultMaxNodes so the node-cap admission gate stays the
	// binding constraint on a normal tree, while a pathological tree
	// (millions of matching files) can no longer grow the enumeration slice
	// without bound before a single node is admitted.
	DefaultMaxRoots = 200000
)

// DefaultOptions returns the recommended bounds for a typical project tree.
func DefaultOptions() Options {
	return Options{MaxDepth: DefaultMaxDepth, MaxNodes: DefaultMaxNodes, MaxFileBytes: DefaultMaxFileBytes, MaxRoots: DefaultMaxRoots}
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
	if o.MaxRoots <= 0 {
		o.MaxRoots = DefaultMaxRoots
	}
	return o
}

// Walk statically resolves the CMake include()/add_subdirectory() graph
// starting at startFile. workspaceRoot is REQUIRED (not optional): it is both
// the hard boundary no resolved path may escape (ReasonOutsideWorkspace
// otherwise) and the value substituted for ${CMAKE_SOURCE_DIR}. Read-only:
// never writes, never executes cmake, never shells out.
//
// ctx bounds the walk: a scan of a pathological tree is abandoned as soon as
// the caller cancels or its deadline expires, and the error is RETURNED
// rather than a partial graph being handed back as if it were complete.
func Walk(ctx context.Context, startFile, workspaceRoot string, opts Options) (*Result, error) {
	if strings.TrimSpace(startFile) == "" {
		return nil, errors.New("cmakegraph: startFile is required")
	}
	w, err := newWalker(ctx, workspaceRoot, opts)
	if err != nil {
		return nil, err
	}
	absStart, err := w.walkRoot(startFile)
	if err != nil {
		return nil, err
	}
	if err := w.ctx.Err(); err != nil {
		return nil, fmt.Errorf("cmakegraph: walk %s: %w", absStart, err)
	}
	return w.result(absStart), nil
}

// WalkTree finds EVERY file under root matching one of entryNames and walks
// each as an independent root, sharing ONE traversal state across all of
// them — so a file is scanned at most once PER DISTINCT SOURCE CONTEXT
// regardless of how many roots would reach it.
//
// workspaceRoot is the boundary + ${CMAKE_SOURCE_DIR} value for every
// constituent walk; it defaults to root when empty. It may be a PARENT of
// root — that is the point of taking it separately: a caller scanning one
// subdirectory of a larger workspace gets includes that reach elsewhere in
// that workspace resolved, instead of falsely reported as escaping it. root
// itself must lie within workspaceRoot, or WalkTree returns an error rather
// than enumerating a tree whose every hit it would then have to refuse.
//
// # Coverage is reported, never assumed
//
// A subtree that cannot be enumerated (an ACL-denied directory) is recorded
// as a CoverageEnumerateFailed hole in Result.UnscannedFiles and enumeration
// continues; a failure to enumerate the ROOT ITSELF is fatal and returns an
// error, because "no matching files" and "could not look" are not the same
// answer. Enumeration is bounded by Options.MaxRoots
// (Result.RootEnumerationCapped) so a tree with millions of matching files
// cannot exhaust memory before the first node is even admitted.
//
// entryNames entries are either an exact basename (case-insensitive, e.g.
// "CMakeLists.txt") or an extension pattern "*.ext" (case-insensitive, e.g.
// "*.cmake"). Passing []string{"*.cmake", "CMakeLists.txt"} scans the WHOLE
// tree's CMake-processable files as independent roots — the right choice for
// a tree with no single top-level CMakeLists.txt (e.g. a vcpkg-style
// overlay-ports tree). A non-CMakeLists.txt root's source-dir context is
// UNVERIFIED (see the package doc); passing []string{"portfile.cmake"}
// reproduces the earlier, narrower "one root per port" behavior with the
// same unverified-context treatment.
//
// Depth (MaxDepth) is bounded PER ROOT, same as a single Walk. MaxNodes is a
// WHOLE-TREE bound shared across every root in this call, enforced at ROOT
// ADMISSION too (see Result.NodeCapTruncated) — not only when following an
// edge within an already-admitted root.
func WalkTree(ctx context.Context, root, workspaceRoot string, entryNames []string, opts Options) (*Result, error) {
	return walkTreeWithOperations(ctx, root, workspaceRoot, entryNames, opts, treeOperations{
		walkDir:            filepath.WalkDir,
		isDirectorySymlink: isDirectorySymlink,
		walkRoot:           (*walker).walkRoot,
	})
}

// treeOperations keeps WalkTree's filesystem and root-processing operations
// local to one invocation. Production passes the ordinary filepath/os-backed
// operations; tests can supply deterministic operations without process-global
// hooks or filesystem privilege requirements.
type treeOperations struct {
	walkDir            func(root string, fn fs.WalkDirFunc) error
	isDirectorySymlink func(path string) bool
	walkRoot           func(*walker, string) (string, error)
}

// walkTreeWithOperations is the package-local WalkTree implementation seam.
// It preserves WalkTree's public contract while allowing tests to control the
// operations whose platform behavior would otherwise make a race-window
// regression nondeterministic.
func walkTreeWithOperations(ctx context.Context, root, workspaceRoot string, entryNames []string, opts Options, operations treeOperations) (*Result, error) {
	if len(entryNames) == 0 {
		return nil, errors.New("cmakegraph: entryNames must be non-empty")
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("cmakegraph: root is required")
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		workspaceRoot = root
	}
	w, err := newWalker(ctx, workspaceRoot, opts)
	if err != nil {
		return nil, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("cmakegraph: resolve tree root: %w", err)
	}
	if !w.withinWorkspace(absRoot) {
		return nil, fmt.Errorf("cmakegraph: tree root %s is outside workspace root %s", absRoot, w.workspaceRoot)
	}

	var starts []string
	rootEnumerationFailed := error(nil)
	walkErr := operations.walkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if path == absRoot {
				// Could not enumerate the ROOT. Returning an empty,
				// apparently-complete graph here would assert "this tree has
				// no CMake files", which we have no evidence for.
				rootEnumerationFailed = err
				return err
			}
			// A subtree we could not look inside. Skipped, but RECORDED —
			// whatever matching files it held are missing from the graph.
			w.recordCoverage(path, CoverageEnumerateFailed, err.Error())
			return nil
		}
		if operations.isDirectorySymlink(path) {
			// A directory symlink is a coverage hole, not a path into another
			// subtree. Lstat identifies the entry itself; os.Stat only classifies
			// its target. On platforms that expose it as a directory, SkipDir is
			// required to prevent descent; elsewhere WalkDir already skips it.
			w.recordCoverage(path, CoverageSymlinkDirectorySkipped, "directory symlink was not traversed")
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !matchesEntry(d.Name(), entryNames) {
			return nil
		}
		if len(starts) >= w.opts.MaxRoots {
			w.rootEnumerationCapped = true
			w.recordCoverage(filepath.Dir(path), CoverageRootEnumerationCapped,
				fmt.Sprintf("stopped enumerating candidate roots at MaxRoots=%d", w.opts.MaxRoots))
			return fs.SkipAll
		}
		starts = append(starts, path)
		return nil
	})
	if rootEnumerationFailed != nil {
		return nil, fmt.Errorf("cmakegraph: enumerate entry files under %s: %w", absRoot, rootEnumerationFailed)
	}
	if walkErr != nil {
		return nil, fmt.Errorf("cmakegraph: enumerate entry files under %s: %w", absRoot, walkErr)
	}
	sort.Strings(starts) // deterministic root order, so a shared file's edge always attributes to the same first-seen context

	for _, s := range starts {
		if err := w.ctx.Err(); err != nil {
			return nil, fmt.Errorf("cmakegraph: walk %s: %w", absRoot, err)
		}
		if _, err := operations.walkRoot(w, s); err != nil {
			return nil, fmt.Errorf("cmakegraph: walk %s: %w", s, err)
		}
	}
	if err := w.ctx.Err(); err != nil {
		return nil, fmt.Errorf("cmakegraph: walk %s: %w", absRoot, err)
	}
	return w.result(absRoot), nil
}

// isDirectorySymlink identifies a symlink entry whose target is a directory.
// Lstat keeps the directory-link policy anchored to the entry being walked;
// Stat is used only to classify the target and never to traverse its contents.
func isDirectorySymlink(path string) bool {
	entry, err := os.Lstat(path)
	if err != nil || entry.Mode()&fs.ModeSymlink == 0 {
		return false
	}
	target, err := os.Stat(path)
	return err == nil && target.IsDir()
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

// sourceContext is this package's tracked approximation of CMake's
// CMAKE_CURRENT_SOURCE_DIR at one point in the traversal, plus whether that
// approximation is VERIFIED (known with confidence) or only a fallback that
// must not be used to resolve a bare-relative argument. See the package
// doc's "Source-dir context must be VERIFIED, never invented" section.
type sourceContext struct {
	dir      string
	verified bool
}

// controlFrame is one lexically-open CMake control construct. It records
// static uncertainty only: scanning never attempts to execute any branch or
// loop. An edge is Conditional while any control frame remains open.
type controlFrame uint8

const (
	controlFrameIf controlFrame = iota
	controlFrameForeach
	controlFrameWhile
)

func closeControlFrame(frames []controlFrame, expected controlFrame) []controlFrame {
	if n := len(frames); n > 0 && frames[n-1] == expected {
		return frames[:n-1]
	}
	return frames
}

// visitKey identifies one (file, source-context) traversal node. Keying by
// context as well as file path is what lets a file reachable under multiple
// DIFFERENT verified contexts be explored under each, rather than a second
// visit being silently suppressed by the first.
type visitKey struct {
	file string
	ctx  sourceContext
}

// dedupCtx normalizes ctx for use as a visitKey. An UNVERIFIED context's dir
// is never actually consulted by classifyArg (a bare-relative argument
// refuses on !ctx.verified before ever reading ctx.dir), so two DIFFERENT
// unverified contexts reaching the same file are operationally
// indistinguishable — collapsing them to one shared key avoids redundant
// re-scans that could only ever reproduce identical results, without
// weakening the P1-3 fix for VERIFIED contexts (which remain fully
// distinct, dir and all).
func dedupCtx(ctx sourceContext) sourceContext {
	if !ctx.verified {
		return sourceContext{verified: false}
	}
	return ctx
}

// walker carries traversal state SHARED across every root walked in one Walk
// or WalkTree call.
type walker struct {
	ctx                   context.Context
	opts                  Options
	workspaceRoot         string // absolute, NOT symlink-resolved (used for ${CMAKE_SOURCE_DIR} substitution)
	realWorkspaceRoot     string // symlink-resolved, used for the boundary check
	visited               map[visitKey]bool
	files                 map[string]bool
	edges                 []Edge
	unscanned             []UnscannedFile
	nodeCapTruncated      bool
	rootsSkippedByNodeCap int
	rootEnumerationCapped bool
}

// newWalker validates workspaceRoot and constructs empty shared traversal state.
func newWalker(ctx context.Context, workspaceRoot string, opts Options) (*walker, error) {
	if ctx == nil {
		return nil, errors.New("cmakegraph: ctx is required")
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil, errors.New("cmakegraph: workspaceRoot is required (it is the hard outside_workspace boundary and the ${CMAKE_SOURCE_DIR} value)")
	}
	normalizedOpts := opts.normalized()
	if err := validateSentinelLimit(normalizedOpts.MaxFileBytes); err != nil {
		return nil, err
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
		ctx:               ctx,
		opts:              normalizedOpts,
		workspaceRoot:     absRoot,
		realWorkspaceRoot: realRoot,
		visited:           map[visitKey]bool{},
		files:             map[string]bool{},
	}, nil
}

// recordCoverage is the SINGLE owner of Result.UnscannedFiles. Every coverage
// hole this package can produce — an unreadable file, an unenumerable
// subtree, a root refused at the workspace boundary, the point enumeration
// was capped — goes through here, so no future path can quietly drop a hole
// on the floor by forgetting to append.
func (w *walker) recordCoverage(path string, reason CoverageReason, detail string) {
	w.unscanned = append(w.unscanned, UnscannedFile{
		Path:   w.canonicalize(path),
		Reason: reason,
		Detail: detail,
	})
}

// walkRoot validates startFile and scans it as a fresh traversal root —
// UNLESS admission is refused because the shared MaxNodes budget is already
// exhausted (Result.NodeCapTruncated / RootsSkippedByNodeCap), or it was
// already visited under the SAME (file, context) key. Its verified-ness is
// CMakeLists.txt-based: a root literally named CMakeLists.txt is treated as
// a genuine directory-scope root (own directory = CMAKE_CURRENT_SOURCE_DIR);
// any other filename (an arbitrary *.cmake discovery root) is UNVERIFIED —
// see the package doc. Returns the absolute path of startFile.
//
// The workspace boundary is enforced HERE, on the CANONICAL (symlink-resolved)
// path, BEFORE os.Stat or any read: an enumerated tree entry may be a symlink
// whose target lies outside the workspace, and opening it to find that out
// would already have leaked the read the boundary exists to prevent. Such a
// root is refused and recorded as a coverage hole, not scanned.
func (w *walker) walkRoot(startFile string) (string, error) {
	absStart, err := filepath.Abs(startFile)
	if err != nil {
		return "", fmt.Errorf("cmakegraph: resolve start file: %w", err)
	}
	canonStart := w.canonicalize(absStart)
	if !w.withinWorkspace(canonStart) {
		w.recordCoverage(canonStart, CoverageRootOutsideWorkspace,
			fmt.Sprintf("root %s resolves outside workspace root %s", canonStart, w.realWorkspaceRoot))
		return absStart, nil
	}
	if fi, statErr := os.Stat(absStart); statErr != nil || fi.IsDir() {
		if statErr != nil {
			return "", fmt.Errorf("cmakegraph: start file %s: %w", absStart, statErr)
		}
		return "", fmt.Errorf("cmakegraph: start file %s is a directory, want a file", absStart)
	}
	verified := strings.EqualFold(filepath.Base(canonStart), "CMakeLists.txt")
	ctx := sourceContext{dir: filepath.Dir(canonStart), verified: verified}
	key := visitKey{file: canonStart, ctx: dedupCtx(ctx)}
	if w.visited[key] {
		return absStart, nil
	}
	if len(w.visited) >= w.opts.MaxNodes {
		w.nodeCapTruncated = true
		w.rootsSkippedByNodeCap++
		return absStart, nil
	}
	w.visited[key] = true
	// w.files is populated inside walkFile, once the read succeeds — see there.
	w.walkFile(absStart, ctx, 0, []string{canonStart})
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
		Root:                  root,
		WorkspaceRoot:         w.workspaceRoot,
		Edges:                 w.edges,
		Files:                 sortedFiles,
		Histogram:             hist,
		NodeCapTruncated:      w.nodeCapTruncated,
		RootsSkippedByNodeCap: w.rootsSkippedByNodeCap,
		RootEnumerationCapped: w.rootEnumerationCapped,
		UnscannedFiles:        w.unscanned,
	}
}

// walkFile scans one file's content for include()/add_subdirectory() calls
// and recurses into newly-resolved, in-budget, non-cyclic targets. ctx is
// this traversal path's current source context (see sourceContext).
// ancestors is the current DFS path (canonical file paths, context-
// independent — see the package doc) used for true-cycle detection.
func (w *walker) walkFile(file string, ctx sourceContext, depth int, ancestors []string) {
	if w.ctx.Err() != nil {
		return
	}
	data, err := readBounded(file, w.opts.MaxFileBytes)
	if err != nil {
		w.recordCoverage(file, readCoverageReason(err), err.Error())
		return
	}
	canonSelf := w.canonicalize(file)
	// Recorded as SCANNED only here, after the read actually succeeded. This is
	// the single owner of w.files: both callers (walkRoot for a root, the
	// resolve loop for an include target) used to insert BEFORE calling
	// walkFile, so a file that exceeded MaxFileBytes or could not be read was
	// listed in files[] as scanned while simultaneously appearing in
	// unscanned_files[] as a coverage hole. Reporting a file as both is worse
	// than either: files[] is what a caller trusts to mean "this was examined".
	w.files[canonSelf] = true
	listDir := filepath.Dir(canonSelf)

	var controlFrames []controlFrame
	macroDepth := 0
	for _, c := range scanTopLevelCommands(data) {
		if w.ctx.Err() != nil {
			return
		}
		switch c.Name {
		case "if":
			controlFrames = append(controlFrames, controlFrameIf)
			continue
		case "endif":
			controlFrames = closeControlFrame(controlFrames, controlFrameIf)
			continue
		case "foreach":
			controlFrames = append(controlFrames, controlFrameForeach)
			continue
		case "endforeach":
			controlFrames = closeControlFrame(controlFrames, controlFrameForeach)
			continue
		case "while":
			controlFrames = append(controlFrames, controlFrameWhile)
			continue
		case "endwhile":
			controlFrames = closeControlFrame(controlFrames, controlFrameWhile)
			continue
		case "macro", "function":
			macroDepth++
			continue
		case "endmacro", "endfunction":
			if macroDepth > 0 {
				macroDepth--
			}
			continue
		}

		var kind EdgeKind
		switch c.Name {
		case "include":
			kind = EdgeInclude
		case "add_subdirectory":
			kind = EdgeAddSubdirectory
		default:
			continue
		}

		var rawArg string
		malformed := c.Malformed
		if !malformed {
			var ok bool
			rawArg, ok = firstArgument(data[c.ArgStart:c.ArgEnd])
			malformed = !ok
		}

		e := Edge{
			Kind:        kind,
			FromFile:    canonSelf,
			Line:        lineOf(data, c.NameOffset),
			RawArg:      rawArg,
			Conditional: len(controlFrames) > 0,
		}

		candidate, reason, ok := classifyArg(kind, rawArg, malformed, macroDepth > 0, listDir, ctx, w.workspaceRoot)
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

		var targetFile string
		var exists, unreadable bool
		var newCtx sourceContext
		switch kind {
		case EdgeInclude:
			fi, statErr := os.Stat(candidate)
			switch {
			case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
				unreadable = true
			case statErr == nil && !fi.IsDir():
				exists = true
				targetFile = candidate
			}
			newCtx = ctx // include() never changes CMAKE_CURRENT_SOURCE_DIR
		case EdgeAddSubdirectory:
			fi, statErr := os.Stat(candidate)
			switch {
			case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
				unreadable = true
			case statErr == nil && fi.IsDir():
				cml := filepath.Join(candidate, "CMakeLists.txt")
				cfi, cerr := os.Stat(cml)
				switch {
				case cerr != nil && !errors.Is(cerr, fs.ErrNotExist):
					unreadable = true
				case cerr == nil && !cfi.IsDir():
					exists = true
					targetFile = cml
				}
			}
			// Reaching this point means `candidate` was computed either from
			// an absolute variable or from an already-verified ctx (see
			// classifyArg) — the child's own source dir is therefore now
			// concretely known, regardless of how uncertain the traversal
			// was before this edge.
			newCtx = sourceContext{dir: candidate, verified: true}
		}

		if unreadable {
			// The filesystem refused to tell us whether the target exists.
			// StatusDangling asserts VERIFIED ABSENCE, which we do not have —
			// fail closed instead (see ReasonTargetUnreadable).
			e.Status = StatusUnresolved
			e.Reason = ReasonTargetUnreadable
			w.edges = append(w.edges, e)
			continue
		}
		if !exists {
			e.Status = StatusDangling
			w.edges = append(w.edges, e)
			continue
		}

		canonTarget := w.canonicalize(targetFile)
		key := visitKey{file: canonTarget, ctx: dedupCtx(newCtx)}
		switch {
		case containsStr(ancestors, canonTarget):
			e.Status = StatusUnresolved
			e.Reason = ReasonCyclic
		case w.visited[key]:
			// Already explored under this EXACT (file, context) — a diamond
			// re-include/re-add from a sibling path under the same context.
			// Not a cycle; not re-scanned (its edges were already
			// discovered). A DIFFERENT context for the same file is NOT
			// suppressed here (see visitKey's doc).
			e.Status = StatusResolved
		case depth+1 > w.opts.MaxDepth || len(w.visited) >= w.opts.MaxNodes:
			e.Status = StatusUnresolved
			e.Reason = ReasonDepthLimit
		default:
			e.Status = StatusResolved
			w.visited[key] = true
			// w.files is populated inside walkFile, once the read succeeds.
			w.edges = append(w.edges, e)
			nextAncestors := make([]string, len(ancestors)+1)
			copy(nextAncestors, ancestors)
			nextAncestors[len(ancestors)] = canonTarget
			w.walkFile(targetFile, newCtx, depth+1, nextAncestors)
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

// errByteCapExceeded is the sentinel readBounded returns when a file is
// larger than Options.MaxFileBytes, so readCoverageReason can classify that
// case by identity (errors.Is) rather than by matching an error STRING.
var errByteCapExceeded = errors.New("cmakegraph: file exceeds byte cap")

// validateSentinelLimit rejects a cap that cannot safely reserve one extra
// byte for the bounded-read sentinel. It is shared by option admission and
// readBoundedFrom so an unrepresentable cap fails before opening or reading a
// file, regardless of which path reaches the reader.
func validateSentinelLimit(maxBytes int64) error {
	if maxBytes == math.MaxInt64 {
		return fmt.Errorf("cmakegraph: max file bytes %d cannot reserve the maxBytes+1 sentinel", maxBytes)
	}
	return nil
}

func readBounded(path string, maxBytes int64) ([]byte, error) {
	if err := validateSentinelLimit(maxBytes); err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("%w: %s (%d > %d)", errByteCapExceeded, path, info.Size(), maxBytes)
	}
	// The Stat above is a TOCTOU window: a file that GROWS between it and the
	// read is the case the LimitReader has to catch. Reading maxBytes exactly
	// could not catch it — a grown file returns exactly maxBytes with a nil
	// error, byte-for-byte indistinguishable from a complete read of a file
	// that happens to be exactly that size. The graph was then built from a
	// SILENTLY TRUNCATED file and reported as complete coverage: every
	// include() past the cut simply did not exist as far as the result was
	// concerned, with nothing in unscanned_files[] to say so.
	//
	// Reading maxBytes+1 makes the overflow observable — the same limit+1 idiom
	// lastfailure's readLogLimited uses — so it can be reported as the byte-cap
	// coverage hole it is.
	return readBoundedFrom(f, path, maxBytes)
}

// readBoundedFrom is the post-Stat read, split out from readBounded so the
// TOCTOU case it exists for is directly exercisable.
//
// The growth race cannot be reproduced deterministically through the
// filesystem: by the time a test could append, the read has happened. Passing
// a reader that yields MORE than the preceding Stat promised is exactly what a
// grown file looks like from here, and is the only honest way to prove the
// bound rather than merely asserting the Stat gate that precedes it.
func readBoundedFrom(r io.Reader, path string, maxBytes int64) ([]byte, error) {
	if err := validateSentinelLimit(maxBytes); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s (grew past %d during read)", errByteCapExceeded, path, maxBytes)
	}
	return data, nil
}

// readCoverageReason maps a readBounded error onto the CLOSED CoverageReason
// enum. Single owner: every caller records the hole through recordCoverage
// with the reason this function returns.
func readCoverageReason(err error) CoverageReason {
	if errors.Is(err, errByteCapExceeded) {
		return CoverageByteCapExceeded
	}
	return CoverageFileUnreadable
}

// --- classification ------------------------------------------------------

// classifyArg applies the resolution precedence documented at the package
// level and returns either a concrete, VERIFIED candidate path (ok=true) or
// an unresolved reason (ok=false, candidate=""). deferredMacro is true when
// this call site is lexically inside an open macro()/function() body (see
// ReasonDeferredMacroContext). ctx is the CURRENT traversal source context
// (see sourceContext) — required (and must be verified) to resolve a
// bare-relative argument.
func classifyArg(kind EdgeKind, rawArg string, malformed, deferredMacro bool, listDir string, ctx sourceContext, workspaceRoot string) (candidate string, reason Reason, ok bool) {
	if malformed || strings.TrimSpace(rawArg) == "" {
		return "", ReasonParseError, false
	}
	// P1-4: escape sequences are refused, never decoded — a decode mistake
	// on a Windows-style path is exactly the false-Resolved class this
	// package must not produce.
	if strings.Contains(rawArg, `\`) {
		return "", ReasonParseError, false
	}
	if strings.Contains(rawArg, "$<") {
		return "", ReasonGeneratorExpression, false
	}
	// P1-2: ${CMAKE_CURRENT_LIST_DIR} is not purely lexical inside a
	// macro()/function() body — refuse rather than substitute the DEFINING
	// file's own directory.
	if deferredMacro && strings.Contains(rawArg, "${CMAKE_CURRENT_LIST_DIR}") {
		return "", ReasonDeferredMacroContext, false
	}
	if strings.Contains(rawArg, "$ENV{") || strings.Contains(rawArg, "$CACHE{") {
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

	p := substituted
	if !filepath.IsAbs(p) {
		// P1-1: a relative argument (no CMAKE_CURRENT_LIST_DIR/
		// CMAKE_SOURCE_DIR was used) resolves against
		// CMAKE_CURRENT_SOURCE_DIR for BOTH include() and add_subdirectory()
		// — see the package doc. This requires a VERIFIED context (P1-3).
		if !ctx.verified {
			return "", ReasonUnverifiedSourceDir, false
		}
		p = filepath.Join(ctx.dir, p)
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

// --- the lexer: one structural pass --------------------------------------

// commandInvocation is one TOP-LEVEL CMake command call discovered by
// scanTopLevelCommands — one that is NOT nested inside another command's own
// argument-list span.
type commandInvocation struct {
	Name       string // lowercased
	NameOffset int    // offset of the command name's first character, used for line numbers
	ArgStart   int    // offset just after '('
	ArgEnd     int    // offset of the matching ')'
	Malformed  bool   // true if no matching ')' was found before EOF
}

// scanTopLevelCommands performs ONE forward pass over data and returns every
// top-level command invocation. See the package doc's "The lexer" section
// for exactly what "top-level" excludes and why.
func scanTopLevelCommands(data []byte) []commandInvocation {
	var cmds []commandInvocation
	i := 0
	for i < len(data) {
		c := data[i]
		switch {
		case isSpace(c):
			i++
		case c == '#':
			if contentStart, eq, ok := matchBracketOpen(data, i+1); ok {
				end := findBracketClose(data, contentStart, eq)
				if end < 0 {
					i = len(data)
				} else {
					i = end
				}
			} else {
				for i < len(data) && data[i] != '\n' {
					i++
				}
			}
		case isIdentStart(c):
			start := i
			for i < len(data) && isIdentCont(data[i]) {
				i++
			}
			name := strings.ToLower(string(data[start:i]))
			j := i
			for j < len(data) && isSpace(data[j]) {
				j++
			}
			if j < len(data) && data[j] == '(' {
				argStart := j + 1
				argEnd, ok := scanArgList(data, argStart)
				cmds = append(cmds, commandInvocation{
					Name: name, NameOffset: start, ArgStart: argStart, ArgEnd: argEnd, Malformed: !ok,
				})
				if ok {
					i = argEnd + 1
				} else {
					// Unbalanced call: stop scanning the rest of this file
					// (see the package doc — no resync heuristic).
					i = len(data)
				}
			}
			// else: a bare identifier with no call following it; already
			// advanced past it, nothing more to do.
		default:
			i++
		}
	}
	return cmds
}

// scanArgList returns the index of the ')' matching the '(' whose contents
// start at `start` (i.e. depth is already 1), correctly treating quoted
// strings, bracket arguments, bracket/line comments, and escaped characters
// as opaque — including any command-name-shaped text within them, which is
// exactly what prevents message(include(x.cmake)) from being misread as a
// real include(). ok is false if no matching close was found before EOF.
func scanArgList(data []byte, start int) (int, bool) {
	depth := 1
	i := start
	for i < len(data) {
		c := data[i]
		switch {
		case c == '"':
			end, ok := skipQuoted(data, i)
			if !ok {
				return len(data), false
			}
			i = end
		case c == '\\':
			i += 2
		case c == '[':
			end, opener, closed := skipBracketArg(data, i)
			switch {
			case !opener:
				i++ // a literal '[' outside any bracket-open sequence
			case !closed:
				// A RECOGNIZED bracket-argument opener with no matching
				// close. Its contents are, by CMake's grammar, uninterpreted
				// text — so falling through to treat the '[' as a literal
				// would hand the payload's own bytes back to the scanner as
				// executable syntax: a ')' inside the payload would close
				// THIS call and whatever followed would be read as a fresh
				// top-level command. The file's syntax is broken from here
				// on; say so and stop, exactly as for an unbalanced paren.
				return len(data), false
			default:
				i = end
			}
		case c == '#':
			if contentStart, eq, ok := matchBracketOpen(data, i+1); ok {
				end := findBracketClose(data, contentStart, eq)
				if end < 0 {
					return len(data), false
				}
				i = end
			} else {
				for i < len(data) && data[i] != '\n' {
					i++
				}
			}
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
			if depth == 0 {
				return i - 1, true
			}
		default:
			i++
		}
	}
	return i, false
}

// skipQuoted returns the index JUST AFTER the closing quote, given
// data[start] == '"', with CORRECT escape parity: a run of N consecutive
// backslashes immediately before a '"' closes the string iff N is even (an
// escaped backslash consumes two bytes atomically, so it can never itself
// "protect" the following character from being read fresh). ok is false if
// no unescaped closing quote is found before EOF.
func skipQuoted(data []byte, start int) (int, bool) {
	i := start + 1
	for i < len(data) {
		switch data[i] {
		case '\\':
			i += 2 // the escaped character (whatever it is) is consumed atomically
		case '"':
			return i + 1, true
		default:
			i++
		}
	}
	return i, false
}

// matchBracketOpen reports whether data[i] begins a CMake bracket sequence
// "[" "="*N "[" , returning the offset just after it (contentStart) and N.
func matchBracketOpen(data []byte, i int) (contentStart, eqCount int, ok bool) {
	if i < 0 || i >= len(data) || data[i] != '[' {
		return 0, 0, false
	}
	j := i + 1
	eq := 0
	for j < len(data) && data[j] == '=' {
		eq++
		j++
	}
	if j < len(data) && data[j] == '[' {
		return j + 1, eq, true
	}
	return 0, 0, false
}

// findBracketClose finds the closing "]" "="*eqCount "]" sequence starting
// the search at `from`, returning the index JUST AFTER it, or -1 if not
// found before EOF.
func findBracketClose(data []byte, from, eqCount int) int {
	if from < 0 || from > len(data) {
		return -1
	}
	closer := "]" + strings.Repeat("=", eqCount) + "]"
	idx := bytes.Index(data[from:], []byte(closer))
	if idx < 0 {
		return -1
	}
	return from + idx + len(closer)
}

// skipBracketArg skips a full bracket-argument span starting at data[i]=='['.
//
// It reports THREE distinct outcomes, because collapsing the last two into a
// single "not ok" is a correctness bug (see scanArgList):
//
//   - opener=false: data[i] does not begin a bracket-open sequence at all.
//     It is an ordinary literal '['; the caller should just step over it.
//   - opener=true, closed=false: a VALID opener whose matching "]"+"="*N+"]"
//     never appears before EOF. The argument is unterminated, so nothing
//     after it can be interpreted — the caller must treat the call as
//     malformed, never resume scanning inside the payload.
//   - opener=true, closed=true: end is the offset just past the close.
func skipBracketArg(data []byte, i int) (end int, opener, closed bool) {
	contentStart, eq, ok := matchBracketOpen(data, i)
	if !ok {
		return i, false, false
	}
	closeEnd := findBracketClose(data, contentStart, eq)
	if closeEnd < 0 {
		return len(data), true, false
	}
	return closeEnd, true, true
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// firstArgument returns the first argument in argText (the raw bytes between
// a call's outer parens), stripping surrounding quotes but NEVER decoding
// escape sequences (the caller refuses any argument containing a backslash —
// see classifyArg). ok is false for an empty/whitespace-only argument list,
// an unterminated quote, or CMake bracket-argument syntax (recognized, but
// its content is never extracted as a path — P1-4).
func firstArgument(argText []byte) (string, bool) {
	// Skip leading whitespace AND comments. scanArgList already understands
	// both CMake comment forms, so a call like
	//
	//	include( # pick the platform variant
	//	        Foo.cmake)
	//
	// is a perfectly ordinary, well-formed call it scans correctly — but this
	// function only skipped whitespace, so it returned "#" as the first
	// argument and the edge was misparsed. The two must agree about what is
	// argument text and what is a comment.
	i := 0
	for i < len(argText) {
		if isSpace(argText[i]) {
			i++
			continue
		}
		if argText[i] != '#' {
			break
		}
		if contentStart, eq, ok := matchBracketOpen(argText, i+1); ok {
			// Bracket comment: #[[ ... ]]. An unterminated one means the rest
			// of the text is uninterpreted content, so there is no first
			// argument to find — malformed, exactly as scanArgList treats it.
			end := findBracketClose(argText, contentStart, eq)
			if end < 0 {
				return "", false
			}
			i = end
			continue
		}
		// Line comment: runs to the newline (or to the end of the arg text).
		for i < len(argText) && argText[i] != '\n' {
			i++
		}
	}
	if i >= len(argText) {
		return "", false
	}
	if argText[i] == '"' {
		end, ok := skipQuoted(argText, i)
		if !ok {
			return "", false
		}
		return string(argText[i+1 : end-1]), true
	}
	if _, _, ok := matchBracketOpen(argText, i); ok {
		return "", false
	}
	start := i
	for i < len(argText) {
		c := argText[i]
		if isSpace(c) {
			break
		}
		if c == '\\' {
			i += 2
			continue
		}
		i++
	}
	if i > len(argText) {
		i = len(argText)
	}
	return string(argText[start:i]), true
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
