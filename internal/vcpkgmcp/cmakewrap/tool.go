// Package cmakewrap is a thin wrapper over mcp-local-hub/internal/cmakegraph
// — the ONE hub-internal dependency this binary is allowed
// (work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md, "Boundary
// with the cmake side"). It surfaces cmakegraph's tri-state resolution,
// Conditional flag, closed Reason enum, and Histogram VERBATIM — it does
// NOT re-implement any include()/add_subdirectory() resolution logic
// itself. The only translation performed here is JSON wire-shaping
// (cmakegraph.Status is a Go int with a String() method; JSON needs the
// string form to stay self-describing/auditable, so that one field is
// converted via cmakegraph's OWN String() method, not a re-implemented
// mapping — Reason, EdgeKind, and Histogram are already string-kinded or
// int-tally types and pass through unchanged).
package cmakewrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Reason is this WRAPPER's own closed enum — distinct from
// cmakegraph.Reason (which is per-edge and surfaced verbatim in Edge.Reason
// below). This one describes why the TOOL CALL itself could not produce a
// result at all (bad arguments, or cmakegraph.Walk/WalkTree returning an
// error), never why an individual edge was unresolved.
type Reason string

const (
	// ReasonArgsInvalid: neither root nor file was supplied, or BOTH were.
	// The two are declared mutually exclusive by the tool schema and they
	// select different traversal modes, so honouring one and discarding the
	// other would silently answer a question the caller did not ask.
	ReasonArgsInvalid Reason = "args_invalid"
	// ReasonWalkFailed: cmakegraph.Walk/WalkTree returned a Go error
	// (e.g. the workspace root does not exist, startFile is empty, the tree
	// root lies outside workspace_root, or the root directory could not be
	// enumerated at all).
	ReasonWalkFailed Reason = "walk_failed"
	// ReasonCanceled: the caller's context was canceled or its deadline
	// expired before the walk finished. Kept distinct from ReasonWalkFailed
	// because it says nothing about the tree — only that we stopped looking.
	ReasonCanceled Reason = "canceled"
)

// Args is the cmake_include_graph tool's input contract.
type Args struct {
	// Root, when set, walks EVERY file under Root matching EntryNames as an
	// independent root (cmakegraph.WalkTree) — the right mode for a tree
	// with no single top-level CMakeLists.txt (e.g. a vcpkg-style
	// overlay-ports tree).
	Root string `json:"root,omitempty"`
	// File, when set (and Root is not), walks starting at this one file
	// (cmakegraph.Walk) — reproduces the narrower "one root per port" mode.
	File string `json:"file,omitempty"`
	// WorkspaceRoot is the boundary no resolved path may escape, and the
	// value substituted for ${CMAKE_SOURCE_DIR}. Defaults to Root when set,
	// else to File's containing directory.
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	// EntryNames is used only in Root mode. Defaults to
	// []string{"CMakeLists.txt", "*.cmake"} — the whole tree's
	// CMake-processable files as independent roots.
	EntryNames []string `json:"entry_names,omitempty"`
	MaxDepth   int      `json:"max_depth,omitempty"`
	MaxNodes   int      `json:"max_nodes,omitempty"`
	// MaxFileBytes bounds a per-file read; 0 uses cmakegraph's own default.
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`
	// MaxRoots bounds how many candidate roots Root mode ENUMERATES before it
	// stops walking (Result.RootEnumerationCapped); 0 uses cmakegraph's own
	// default. Separate from MaxNodes, which bounds admission, not discovery.
	MaxRoots int `json:"max_roots,omitempty"`
}

// Edge is the JSON wire shape of one cmakegraph.Edge. Every field maps
// 1:1 to its cmakegraph counterpart; Status is the only one translated
// (int -> its own String() form) for JSON self-description.
type Edge struct {
	Kind         cmakegraph.EdgeKind `json:"kind"`
	FromFile     string              `json:"from_file"`
	Line         int                 `json:"line"`
	RawArg       string              `json:"raw_arg"`
	Status       string              `json:"status"`
	Reason       cmakegraph.Reason   `json:"reason,omitempty"`
	ResolvedPath string              `json:"resolved_path,omitempty"`
	Conditional  bool                `json:"conditional"`
}

func toWireEdge(e cmakegraph.Edge) Edge {
	return Edge{
		Kind:         e.Kind,
		FromFile:     e.FromFile,
		Line:         e.Line,
		RawArg:       e.RawArg,
		Status:       e.Status.String(),
		Reason:       e.Reason,
		ResolvedPath: e.ResolvedPath,
		Conditional:  e.Conditional,
	}
}

// Result is the cmake_include_graph tool's output contract.
type Result struct {
	// Status/Reason describe whether THIS TOOL CALL produced a result at
	// all — never a per-edge outcome (see Edge.Status for that, surfaced
	// verbatim per-edge exactly as cmakegraph computed it).
	Status Status `json:"status"`
	Reason Reason `json:"reason,omitempty"`

	Root          string   `json:"root,omitempty"`
	WorkspaceRoot string   `json:"workspace_root,omitempty"`
	Edges         []Edge   `json:"edges,omitempty"`
	Files         []string `json:"files,omitempty"`
	// Histogram is cmakegraph.Histogram VERBATIM — the operator-facing
	// go/no-go metric this tool must not re-derive or re-interpret.
	Histogram cmakegraph.Histogram `json:"histogram"`

	// The three fields below are cmakegraph's own COVERAGE-HONESTY signals,
	// forwarded verbatim. They state where the walk stopped short, so a
	// caller never mistakes a truncated graph for a complete one.
	//
	// NodeCapTruncated / RootsSkippedByNodeCap: at least one independent
	// root was refused admission because the shared node budget was already
	// exhausted — edges from it are MISSING, not absent.
	NodeCapTruncated      bool `json:"node_cap_truncated"`
	RootsSkippedByNodeCap int  `json:"roots_skipped_by_node_cap,omitempty"`
	// RootEnumerationCapped: root-mode discovery stopped at max_roots, so
	// candidate roots beyond it were never even enumerated — the count is
	// unknowable by construction, unlike roots_skipped_by_node_cap.
	RootEnumerationCapped bool `json:"root_enumeration_capped"`
	// UnscannedFiles lists every COVERAGE HOLE cmakegraph recorded: a file
	// whose content could not be read (byte cap, permission error, race), a
	// subtree that could not be enumerated, a root refused for escaping
	// workspace_root, and the point enumeration was capped. Whatever is
	// behind each entry is unknown, not zero. Each carries a CLOSED
	// cmakegraph.CoverageReason plus a human-only detail string.
	UnscannedFiles []cmakegraph.UnscannedFile `json:"unscanned_files,omitempty"`

	Evidence evidence.Evidence `json:"evidence"`
}

// Status aliases evidence.Status so callers of this package do not need a
// second import just to read Result.Status.
type Status = evidence.Status

// walkFn/walkTreeFn let tests substitute cmakegraph without touching the
// real filesystem — the same determinism seam used by discovery and
// lastfailure. Production callers use RunGraph, which wires the real
// cmakegraph.Walk/WalkTree.
type walkFn func(ctx context.Context, startFile, workspaceRoot string, opts cmakegraph.Options) (*cmakegraph.Result, error)
type walkTreeFn func(ctx context.Context, root, workspaceRoot string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error)

// RunGraph executes cmake_include_graph against the real cmakegraph
// package. Kept as a thin entry point so the server package has one call
// site; run is the fully-injectable implementation tests exercise.
func RunGraph(ctx context.Context, args Args) Result {
	return run(ctx, args, cmakegraph.Walk, cmakegraph.WalkTree)
}

func run(ctx context.Context, args Args, walk walkFn, walkTree walkTreeFn) Result {
	root := strings.TrimSpace(args.Root)
	file := strings.TrimSpace(args.File)

	// root and file are declared MUTUALLY EXCLUSIVE by the tool schema and
	// select different traversal modes. Supplying both is not a caller
	// preference to be resolved silently in root's favour — it is a request
	// whose intent we cannot know, so it is refused rather than answered
	// with an "ok" that analysed a tree the caller may not have meant.
	if root != "" && file != "" {
		var ev evidence.Evidence
		ev.AddPath(root)
		ev.AddPath(file)
		return Result{Status: evidence.StatusUnknown, Reason: ReasonArgsInvalid, Evidence: ev}
	}
	if root == "" && file == "" {
		return Result{Status: evidence.StatusUnknown, Reason: ReasonArgsInvalid}
	}

	workspaceRoot := strings.TrimSpace(args.WorkspaceRoot)
	if workspaceRoot == "" {
		if root != "" {
			workspaceRoot = root
		} else {
			// filepath.Dir, never hand-rolled separator arithmetic: for a
			// root-level file, "C:\CMakeLists.txt" must yield "C:\" (not the
			// drive-relative "C:", whose meaning depends on the process's
			// per-drive working directory) and "/CMakeLists.txt" must yield
			// the root (not "").
			workspaceRoot = filepath.Dir(file)
		}
	}

	opts := cmakegraph.Options{
		MaxDepth:     args.MaxDepth,
		MaxNodes:     args.MaxNodes,
		MaxFileBytes: args.MaxFileBytes,
		MaxRoots:     args.MaxRoots,
	}

	var cgResult *cmakegraph.Result
	var err error
	if root != "" {
		entryNames := args.EntryNames
		if len(entryNames) == 0 {
			entryNames = []string{"CMakeLists.txt", "*.cmake"}
		}
		// workspaceRoot is passed through, NOT re-derived from root: it is
		// both the escape boundary and the ${CMAKE_SOURCE_DIR} value, so
		// pinning it to root would report includes that legitimately reach
		// elsewhere in the caller's workspace as escaping it.
		cgResult, err = walkTree(ctx, root, workspaceRoot, entryNames, opts)
	} else {
		cgResult, err = walk(ctx, file, workspaceRoot, opts)
	}

	if err != nil {
		var ev evidence.Evidence
		ev.AddPath(root)
		ev.AddPath(file)
		ev.AddPath(workspaceRoot)
		reason := ReasonWalkFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = ReasonCanceled
		}
		return Result{Status: evidence.StatusUnknown, Reason: reason, Evidence: ev}
	}

	var ev evidence.Evidence
	ev.AddPath(cgResult.Root)
	ev.AddPath(cgResult.WorkspaceRoot)
	for _, f := range cgResult.Files {
		ev.AddPath(f)
	}

	edges := make([]Edge, 0, len(cgResult.Edges))
	for _, e := range cgResult.Edges {
		edges = append(edges, toWireEdge(e))
	}

	return Result{
		Status:                evidence.StatusOK,
		Root:                  cgResult.Root,
		WorkspaceRoot:         cgResult.WorkspaceRoot,
		Edges:                 edges,
		Files:                 cgResult.Files,
		Histogram:             cgResult.Histogram,
		NodeCapTruncated:      cgResult.NodeCapTruncated,
		RootsSkippedByNodeCap: cgResult.RootsSkippedByNodeCap,
		RootEnumerationCapped: cgResult.RootEnumerationCapped,
		UnscannedFiles:        cgResult.UnscannedFiles,
		Evidence:              ev,
	}
}
