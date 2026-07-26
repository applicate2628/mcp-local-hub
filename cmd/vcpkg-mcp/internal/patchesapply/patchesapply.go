// Package patchesapply answers, from a portfile.cmake alone and WITHOUT
// applying anything or fetching sources: which patches would actually be
// applied for a given triplet, in what order, and does the declared set
// match what is on disk. See
// work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md ("4.
// vcpkg_patches_apply(port) — correct semantics") and
// work-items/decisions/2026-07-25-vcpkg-ground-truth-measured.md (§3, §7)
// for the measured traps this package is built against:
//
//   - A patch reference is resolved (${VAR} expansion via set()/
//     get_filename_component(), plus the ${PORT}/${CURRENT_PORT_DIR}
//     builtins and $ENV{VCPKG_ROOT}) BEFORE judging it present — a resolved
//     path may point OUTSIDE the port directory entirely (the licensepp
//     shape). A port-dir-relative assumption reports a healthy port as a
//     hard defect.
//   - PATCHES is often built CONDITIONALLY via list(APPEND ...) inside
//     if()/elseif()/else() blocks, so the applied SET depends on the
//     triplet/toolchain (the python3 shape) — reading the directory listing
//     is meaningless; this package resolves the declared list for the
//     GIVEN triplet.
//   - Physical files on disk and portfile references diverge in BOTH
//     directions: a referenced patch can be absent (missing — a real
//     defect) and a physical file can be unreferenced (orphaned — a
//     finding, not an error; some portfiles declare no PATCHES at all while
//     patch files still sit in the port directory, e.g. netgen).
//
// This package is a pure text-and-filesystem analysis: it never invokes
// cmake, vcpkg, git apply, or patch, and never fetches or extracts
// anything (see work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md,
// "Read-only by default").
//
// Behavioural invariant shared with every vcpkg-mcp tool: every result is
// tri-state (evidence.StatusOK/Failed/Unknown) and a guard this package
// cannot resolve from the triplet name or an explicit override becomes
// Tri Unknown ("undecidable"), never a guessed true/false — see tribool.go.
package patchesapply

import (
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
)

// Args is the input contract for ApplyOrder.
type Args struct {
	// PortDir is the absolute path to the port directory (containing
	// portfile.cmake). Required.
	PortDir string `json:"port_dir"`
	// Triplet is the triplet name to resolve guards against (e.g.
	// "x64-windows-static", "x64-mingw-dynamic"). Required.
	Triplet string `json:"triplet"`
	// VcpkgRoot backs $ENV{VCPKG_ROOT} expansion inside the portfile. Needed
	// only when a portfile's patch resolution chain reads it (the
	// "${...}/ports/${PORT}" style path the licensepp shape uses).
	VcpkgRoot string `json:"vcpkg_root,omitempty"`
	// PortName overrides the ${PORT} builtin; defaults to
	// filepath.Base(PortDir) when omitted.
	PortName string `json:"port_name,omitempty"`
	// VarOverrides supplies explicit values for variables this package
	// cannot derive from Triplet alone (VCPKG_CROSSCOMPILING, WINSDK_VERSION,
	// ...) or to force a derivable one (VCPKG_LIBRARY_LINKAGE). Values use
	// CMake's own truthy spelling (e.g. "ON"/"OFF", "static"/"dynamic") —
	// see tribool.go's truthy() for the exact constant set.
	VarOverrides map[string]string `json:"var_overrides,omitempty"`
}

// Reason is populated only when Result.Status == evidence.StatusUnknown or
// StatusFailed. Closed enum.
type Reason string

const (
	// ReasonEmptyPortDir: PortDir was empty/whitespace-only. Failed (bad
	// caller input, not an environment condition).
	ReasonEmptyPortDir Reason = "empty_port_dir"
	// ReasonEmptyTriplet: Triplet was empty/whitespace-only. Failed.
	ReasonEmptyTriplet Reason = "empty_triplet"
	// ReasonPortDirMissing: PortDir does not exist or is not a directory.
	ReasonPortDirMissing Reason = "port_dir_missing"
	// ReasonPortfileUnreadable: PortDir exists but portfile.cmake could not
	// be read (missing, permission denied, ...).
	ReasonPortfileUnreadable Reason = "portfile_unreadable"
	// ReasonNoPatchesDeclared: the portfile never references a PATCHES
	// keyword-arg at all (the netgen shape) — every physical .patch/.diff
	// file in the port directory is therefore orphaned by construction.
	ReasonNoPatchesDeclared Reason = "no_patches_declared"
	// ReasonPatchesExprUnparsable: the portfile's statement structure itself
	// is broken (unbalanced parentheses before EOF), so no extracted entry
	// can be trusted. A single unparsable if()-condition INSIDE an otherwise
	// well-formed file does NOT produce this — that guard degrades to the
	// per-entry Undecidable bucket instead (see condition.go).
	ReasonPatchesExprUnparsable Reason = "patches_expression_unparsable"
)

// AppliedPatch is one patch that WOULD be applied for this triplet, in the
// exact order vcpkg would apply it (0-based Ordinal, matching the observed
// real-world patch-<triplet>-<N>-{out,err}.log naming).
type AppliedPatch struct {
	Ordinal      int    `json:"ordinal"`
	Filename     string `json:"filename"`
	ResolvedPath string `json:"resolved_path"`
	Exists       bool   `json:"exists"`
	// Guard is the verbatim enclosing guard chain; empty means unconditional.
	Guard string `json:"guard,omitempty"`
}

// ConditionalPatch is a declared patch whose guard is definitively FALSE for
// this triplet — it would not be applied.
type ConditionalPatch struct {
	Filename     string `json:"filename"`
	ResolvedPath string `json:"resolved_path"`
	Guard        string `json:"guard"`
}

// UndecidablePatch is a declared patch whose guard could not be decided —
// neither true nor false — from the triplet name or supplied overrides.
type UndecidablePatch struct {
	Filename       string   `json:"filename"`
	ResolvedPath   string   `json:"resolved_path"`
	Guard          string   `json:"guard"`
	UnresolvedVars []string `json:"unresolved_vars,omitempty"`
}

// MissingPatch is any declared reference (from Applied, ConditionalNotApplied,
// or Undecidable — the guard's truth does not matter here) whose resolved
// path does not exist on disk: "referenced but absent after resolution".
type MissingPatch struct {
	Filename     string `json:"filename"`
	ResolvedPath string `json:"resolved_path"`
	Guard        string `json:"guard,omitempty"`
}

// OrphanedPatch is a .patch/.diff file physically present in the port
// directory that no PATCHES reference — in any branch, applied or not —
// ever names. A finding, not an error.
type OrphanedPatch struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
}

// Result is the outcome of one ApplyOrder call.
type Result struct {
	Status Status `json:"status"`
	Reason Reason `json:"reason,omitempty"`

	Triplet string `json:"triplet,omitempty"`
	PortDir string `json:"port_dir,omitempty"`

	Applied               []AppliedPatch     `json:"applied,omitempty"`
	ConditionalNotApplied []ConditionalPatch `json:"conditional_not_applied,omitempty"`
	Undecidable           []UndecidablePatch `json:"undecidable,omitempty"`
	Orphaned              []OrphanedPatch    `json:"orphaned,omitempty"`
	Missing               []MissingPatch     `json:"missing,omitempty"`

	Evidence evidence.Evidence `json:"evidence"`
}

// Status is a local alias so callers of this package do not need a second
// import just to read Result.Status.
type Status = evidence.Status

// Deps abstracts every ambient input (filesystem) behind injectable
// functions, per the repo-wide determinism/ambient-input-control rule —
// tests exercise the exact same code path with t.TempDir() fixtures, never
// a real vcpkg checkout.
type Deps struct {
	Stat     func(path string) (os.FileInfo, error)
	ReadDir  func(path string) ([]os.DirEntry, error)
	ReadFile func(path string) ([]byte, error)
}

// DefaultDeps wires Deps to the real OS. Production callers use this; tests
// build their own Deps (or just point Args.PortDir at a t.TempDir() fixture
// and use DefaultDeps, which is simplest — nothing here is mocked out at
// the OS boundary, only the environment/inputs are fixture-controlled).
func DefaultDeps() Deps {
	return Deps{
		Stat:     os.Stat,
		ReadDir:  os.ReadDir,
		ReadFile: os.ReadFile,
	}
}

// ApplyOrder executes vcpkg_patches_apply against the real filesystem.
func ApplyOrder(args Args) Result {
	return applyOrder(args, DefaultDeps())
}

func applyOrder(args Args, deps Deps) Result {
	var ev evidence.Evidence

	portDir := strings.TrimSpace(args.PortDir)
	triplet := strings.TrimSpace(args.Triplet)

	if portDir == "" {
		return Result{Status: evidence.StatusFailed, Reason: ReasonEmptyPortDir, Evidence: ev}
	}
	if triplet == "" {
		return Result{Status: evidence.StatusFailed, Reason: ReasonEmptyTriplet, Evidence: ev}
	}

	ev.AddPath(portDir)
	fi, err := deps.Stat(portDir)
	if err != nil || !fi.IsDir() {
		return Result{
			Status: evidence.StatusUnknown, Reason: ReasonPortDirMissing,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
		}
	}

	portfilePath := filepath.Join(portDir, "portfile.cmake")
	ev.AddPath(portfilePath)
	raw, err := deps.ReadFile(portfilePath)
	if err != nil {
		return Result{
			Status: evidence.StatusUnknown, Reason: ReasonPortfileUnreadable,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
		}
	}
	ev.AddCommand("read " + portfilePath)

	portName := strings.TrimSpace(args.PortName)
	if portName == "" {
		portName = filepath.Base(portDir)
	}
	env := newVarEnv(portDir, portName, strings.TrimSpace(args.VcpkgRoot), args.VarOverrides, triplet)

	entries, sawPatches, truncated := walkPortfile(string(raw), env)
	if truncated {
		return Result{
			Status: evidence.StatusUnknown, Reason: ReasonPatchesExprUnparsable,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
		}
	}

	res := Result{Triplet: triplet, PortDir: portDir}
	referenced := map[string]bool{}
	ordinal := 0
	for _, e := range entries {
		resolvedPath := resolvePatchPath(e.expanded, portDir)
		exists := pathExists(deps, resolvedPath)
		if resolvedPath != "" {
			referenced[filepath.Clean(resolvedPath)] = true
		}

		switch e.guard {
		case TriTrue:
			res.Applied = append(res.Applied, AppliedPatch{
				Ordinal:      ordinal,
				Filename:     e.raw,
				ResolvedPath: resolvedPath,
				Exists:       exists,
				Guard:        e.guardText,
			})
			ordinal++
		case TriFalse:
			res.ConditionalNotApplied = append(res.ConditionalNotApplied, ConditionalPatch{
				Filename:     e.raw,
				ResolvedPath: resolvedPath,
				Guard:        e.guardText,
			})
		default: // TriUnknown
			res.Undecidable = append(res.Undecidable, UndecidablePatch{
				Filename:       e.raw,
				ResolvedPath:   resolvedPath,
				Guard:          e.guardText,
				UnresolvedVars: e.unresolvedVars,
			})
		}
		if !exists {
			res.Missing = append(res.Missing, MissingPatch{
				Filename: e.raw, ResolvedPath: resolvedPath, Guard: e.guardText,
			})
		}
	}

	res.Orphaned = findOrphans(deps, portDir, referenced)
	res.Evidence = ev

	if !sawPatches {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNoPatchesDeclared
		return res
	}
	res.Status = evidence.StatusOK
	return res
}

// resolvePatchPath turns an ${VAR}-expanded (but not yet path-joined)
// reference into a final absolute path: already-absolute expansions
// (the licensepp shape) are used as-is (cleaned); relative ones are
// resolved against portDir. An empty expansion yields an empty path (never
// resolved to portDir itself).
func resolvePatchPath(expanded, portDir string) string {
	if expanded == "" {
		return ""
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded)
	}
	return filepath.Clean(filepath.Join(portDir, expanded))
}

func pathExists(deps Deps, path string) bool {
	if path == "" {
		return false
	}
	fi, err := deps.Stat(path)
	return err == nil && !fi.IsDir()
}

// findOrphans scans portDir's top-level entries for .patch/.diff files
// whose cleaned absolute path never appeared in referenced (any declared
// entry, regardless of guard truth — an orphan is orphaned for every
// triplet, not just this one). A ReadDir failure degrades to "no orphans
// reported" rather than failing the whole call — the applied/conditional/
// undecidable/missing buckets are already independently useful.
func findOrphans(deps Deps, portDir string, referenced map[string]bool) []OrphanedPatch {
	dirEntries, err := deps.ReadDir(portDir)
	if err != nil {
		return nil
	}
	var out []OrphanedPatch
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".patch") && !strings.HasSuffix(name, ".diff") {
			continue
		}
		full := filepath.Clean(filepath.Join(portDir, name))
		if !referenced[full] {
			out = append(out, OrphanedPatch{Filename: name, Path: full})
		}
	}
	return out
}
