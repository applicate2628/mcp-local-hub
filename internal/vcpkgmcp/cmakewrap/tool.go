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
	// ReasonArgsInvalid: neither root nor file was supplied, or file/root
	// were both supplied to inconsistent effect.
	ReasonArgsInvalid Reason = "args_invalid"
	// ReasonWalkFailed: cmakegraph.Walk/WalkTree returned a Go error
	// (e.g. the workspace root does not exist, or startFile is empty).
	ReasonWalkFailed Reason = "walk_failed"
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
	// UnscannedFiles lists files whose CONTENT could not be read (byte cap,
	// permission error, race). Their includes are unknown, not zero.
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
type walkFn func(startFile, workspaceRoot string, opts cmakegraph.Options) (*cmakegraph.Result, error)
type walkTreeFn func(root string, entryNames []string, opts cmakegraph.Options) (*cmakegraph.Result, error)

// RunGraph executes cmake_include_graph against the real cmakegraph
// package. Kept as a thin entry point so the server package has one call
// site; run is the fully-injectable implementation tests exercise.
func RunGraph(args Args) Result {
	return run(args, cmakegraph.Walk, cmakegraph.WalkTree)
}

func run(args Args, walk walkFn, walkTree walkTreeFn) Result {
	root := strings.TrimSpace(args.Root)
	file := strings.TrimSpace(args.File)

	if root == "" && file == "" {
		return Result{Status: evidence.StatusUnknown, Reason: ReasonArgsInvalid}
	}

	workspaceRoot := strings.TrimSpace(args.WorkspaceRoot)
	if workspaceRoot == "" {
		if root != "" {
			workspaceRoot = root
		} else {
			workspaceRoot = parentDir(file)
		}
	}

	opts := cmakegraph.Options{
		MaxDepth:     args.MaxDepth,
		MaxNodes:     args.MaxNodes,
		MaxFileBytes: args.MaxFileBytes,
	}

	var cgResult *cmakegraph.Result
	var err error
	if root != "" {
		entryNames := args.EntryNames
		if len(entryNames) == 0 {
			entryNames = []string{"CMakeLists.txt", "*.cmake"}
		}
		cgResult, err = walkTree(root, entryNames, opts)
	} else {
		cgResult, err = walk(file, workspaceRoot, opts)
	}

	if err != nil {
		var ev evidence.Evidence
		ev.AddPath(root)
		ev.AddPath(file)
		ev.AddPath(workspaceRoot)
		return Result{Status: evidence.StatusUnknown, Reason: ReasonWalkFailed, Evidence: ev}
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
		UnscannedFiles:        cgResult.UnscannedFiles,
		Evidence:              ev,
	}
}

func parentDir(file string) string {
	i := strings.LastIndexAny(file, `/\`)
	if i < 0 {
		return "."
	}
	return file[:i]
}
