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
	"sort"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Args is the input contract for ApplyOrder.
type Args struct {
	// PortDir is the absolute path to the port directory (containing
	// portfile.cmake). Required.
	PortDir string `json:"port_dir"`
	// Triplet is the triplet name to resolve guards against (e.g.
	// "x64-windows-static", "x64-mingw-dynamic"). Required.
	Triplet string `json:"triplet"`
	// VcpkgRoot backs $ENV{VCPKG_ROOT} expansion inside the portfile, and
	// supplies the builtin triplet lookup roots <root>/triplets and
	// <root>/triplets/community. Must be absolute; a relative value is
	// ignored for triplet lookup rather than bound to the daemon's working
	// directory.
	VcpkgRoot string `json:"vcpkg_root,omitempty"`
	// OverlayTriplets is the ordered --overlay-triplets chain (order IS
	// precedence, first match wins), taken from vcpkg's own key rather than
	// guessed from machine layout. It is how a CUSTOM triplet's real
	// variables reach this evaluation: without a triplet file, every triplet
	// variable is unresolved and every guard depending on one becomes
	// undecidable. Entries must be absolute.
	OverlayTriplets []string `json:"overlay_triplets,omitempty"`
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

	// --- Evidence-integrity reasons -------------------------------------
	// Each of the three below reports "the filesystem declined to answer a
	// question this result depends on". All THREE preserve every bucket
	// computed so far — a partial inventory plus an honest unknown is more
	// useful than either a silent partial answer or a bare refusal — and all
	// three list the offending paths in Unreadable.

	// ReasonTripletFileUnreadable: a triplet-file candidate exists on the
	// lookup path but could not be read, so the triplet variables governing
	// every guard are unknown for a reason that is fixable (an ACL, a lock)
	// rather than inherent.
	ReasonTripletFileUnreadable Reason = "triplet_file_unreadable"
	// ReasonPatchPathUnreadable: at least one declared patch path could not
	// be probed. Such a path is NOT in Missing — "missing" is a real defect
	// report, and a permission error is not evidence that a file is absent.
	ReasonPatchPathUnreadable Reason = "patch_path_unreadable"
	// ReasonOrphanScanIncomplete: at least one directory under the port dir
	// could not be listed, so the orphan inventory is a PREFIX of the truth.
	// Without this, an unreadable subdirectory full of unreferenced patches
	// was indistinguishable from "no orphans", under an overall ok.
	ReasonOrphanScanIncomplete Reason = "orphan_scan_incomplete"
)

// UnreadableKind names which question a probe failed to answer. Closed enum.
type UnreadableKind string

const (
	UnreadablePatchPath   UnreadableKind = "patch_path"
	UnreadableOrphanDir   UnreadableKind = "orphan_scan_dir"
	UnreadableTripletFile UnreadableKind = "triplet_file"
)

// UnreadablePath is one path whose existence or contents could not be
// established. Distinct from Missing (verified absent) on purpose: the two
// demand different operator responses, and conflating them turns an
// environment problem into a false bug report about the port.
type UnreadablePath struct {
	Path string         `json:"path"`
	Kind UnreadableKind `json:"kind"`
	// Error is the underlying OS error text — free-form diagnostic detail,
	// never a verdict (verdicts stay in the closed Reason/Kind enums).
	Error string `json:"error,omitempty"`
}

// AppliedPatch is one patch that WOULD be applied for this triplet, in the
// exact order vcpkg would apply it (0-based Ordinal, matching the observed
// real-world patch-<triplet>-<N>-{out,err}.log naming).
type AppliedPatch struct {
	Ordinal      int    `json:"ordinal"`
	Filename     string `json:"filename"`
	ResolvedPath string `json:"resolved_path"`
	// Existence is tri-state, replacing a bool that reported an
	// access-denied Stat as a verified-absent patch file. "unreadable" means
	// the probe failed, not that the patch is gone.
	Existence evidence.Presence `json:"existence"`
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
	// TripletFile is the triplet file whose set() calls established the
	// triplet variables, when one was found. Empty means NO triplet file was
	// available, so every triplet variable was unresolved — the single most
	// useful thing to check when guards come back undecidable unexpectedly.
	TripletFile string `json:"triplet_file,omitempty"`

	Applied               []AppliedPatch     `json:"applied,omitempty"`
	ConditionalNotApplied []ConditionalPatch `json:"conditional_not_applied,omitempty"`
	Undecidable           []UndecidablePatch `json:"undecidable,omitempty"`
	Orphaned              []OrphanedPatch    `json:"orphaned,omitempty"`
	Missing               []MissingPatch     `json:"missing,omitempty"`
	// Unreadable lists every path the filesystem refused to answer for. Any
	// entry here forces Status=unknown with the matching Reason.
	Unreadable []UnreadablePath `json:"unreadable,omitempty"`

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
	vcpkgRoot := strings.TrimSpace(args.VcpkgRoot)

	// Triplet variables come from an ACTUAL triplet file, never from the
	// triplet name — see triplet.go. No file found means every triplet
	// variable stays unresolved, which surfaces as undecidable guards rather
	// than as invented ON/OFF/static/dynamic values.
	var unreadable []UnreadablePath
	var tripletFacts map[string]string
	tripletFile, tripletPresence, tripletErr := resolveTripletFile(deps, triplet, args.OverlayTriplets, vcpkgRoot)
	switch tripletPresence {
	case evidence.PresenceExists:
		ev.AddPath(tripletFile)
		tripletRaw, terr := deps.ReadFile(tripletFile)
		if terr != nil {
			unreadable = append(unreadable, UnreadablePath{
				Path: tripletFile, Kind: UnreadableTripletFile, Error: terr.Error(),
			})
		} else {
			ev.AddCommand("read " + tripletFile)
			tripletFacts = parseTripletFacts(string(tripletRaw), portDir, portName, vcpkgRoot)
		}
	case evidence.PresenceUnreadable:
		errText := ""
		if tripletErr != nil {
			errText = tripletErr.Error()
		}
		unreadable = append(unreadable, UnreadablePath{
			Path: tripletFile, Kind: UnreadableTripletFile, Error: errText,
		})
	}

	env := newVarEnv(portDir, portName, vcpkgRoot, args.VarOverrides, tripletFacts)

	entries, sawPatches, truncated := walkPortfile(string(raw), env)
	if truncated {
		return Result{
			Status: evidence.StatusUnknown, Reason: ReasonPatchesExprUnparsable,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
		}
	}

	res := Result{Triplet: triplet, PortDir: portDir, TripletFile: tripletFile}
	if tripletPresence != evidence.PresenceExists {
		res.TripletFile = ""
	}
	referenced := map[string]bool{}
	ordinal := 0
	for _, e := range entries {
		resolvedPath := resolvePatchPath(e.expanded, portDir)
		pathUnresolved := dedupStrings(e.pathUnresolved)
		existence := evidence.PresenceAbsent
		if len(pathUnresolved) == 0 {
			var perr error
			existence, perr = probePatchFile(deps, resolvedPath)
			if existence == evidence.PresenceUnreadable {
				errText := ""
				if perr != nil {
					errText = perr.Error()
				}
				unreadable = append(unreadable, UnreadablePath{
					Path: resolvedPath, Kind: UnreadablePatchPath, Error: errText,
				})
			}
		}
		if resolvedPath != "" && len(pathUnresolved) == 0 {
			referenced[filepath.Clean(resolvedPath)] = true
		}

		// A path that retains an unresolved variable cannot be classified as
		// applied or missing: either assertion would claim filesystem knowledge
		// the evaluator does not have. Path uncertainty takes precedence over a
		// decidable control-flow guard.
		if len(pathUnresolved) != 0 {
			res.Undecidable = append(res.Undecidable, UndecidablePatch{
				Filename:       e.raw,
				ResolvedPath:   resolvedPath,
				Guard:          e.guardText,
				UnresolvedVars: dedupStrings(append(append([]string{}, e.unresolvedVars...), pathUnresolved...)),
			})
			continue
		}

		switch e.guard {
		case TriTrue:
			res.Applied = append(res.Applied, AppliedPatch{
				Ordinal:      ordinal,
				Filename:     e.raw,
				ResolvedPath: resolvedPath,
				Existence:    existence,
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
		// Only a VERIFIED absence is a missing patch. An unreadable path is
		// reported in Unreadable instead: calling it "missing" would turn an
		// ACL problem into a false bug report against the port.
		if existence == evidence.PresenceAbsent {
			res.Missing = append(res.Missing, MissingPatch{
				Filename: e.raw, ResolvedPath: resolvedPath, Guard: e.guardText,
			})
		}
	}

	orphans, orphanFailures := findOrphans(deps, portDir, referenced)
	res.Orphaned = orphans
	unreadable = append(unreadable, orphanFailures...)
	res.Unreadable = unreadable
	res.Evidence = ev

	// Verdict precedence: evidence-integrity problems first (each names a
	// different remedy), then the structural "nothing declared" case. Every
	// branch keeps the buckets already computed.
	switch {
	case hasUnreadableKind(unreadable, UnreadableTripletFile):
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonTripletFileUnreadable
	case hasUnreadableKind(unreadable, UnreadablePatchPath):
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPatchPathUnreadable
	case hasUnreadableKind(unreadable, UnreadableOrphanDir):
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonOrphanScanIncomplete
	case !sawPatches:
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNoPatchesDeclared
	default:
		res.Status = evidence.StatusOK
	}
	return res
}

func hasUnreadableKind(in []UnreadablePath, kind UnreadableKind) bool {
	for _, u := range in {
		if u.Kind == kind {
			return true
		}
	}
	return false
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

// probePatchFile reports whether a resolved patch reference is present on
// disk, TRI-STATE. An empty path (an expansion that produced nothing) is a
// verified absence, not a probe failure.
func probePatchFile(deps Deps, path string) (evidence.Presence, error) {
	if path == "" {
		return evidence.PresenceAbsent, nil
	}
	return evidence.ProbeFile(evidence.StatFunc(deps.Stat), path)
}

// findOrphans recursively scans portDir for .patch/.diff files whose cleaned
// absolute path never appeared in referenced (any declared entry, regardless
// of guard truth — an orphan is orphaned for every triplet, not just this
// one).
//
// A ReadDir failure is now ACCUMULATED and returned rather than silently
// ending that branch of the walk. Swallowing it produced an empty (or
// truncated) orphan list that was indistinguishable from a verified "no
// orphans", under an overall ok — the caller had no way to tell that a
// subdirectory full of unreferenced patches simply could not be listed.
func findOrphans(deps Deps, portDir string, referenced map[string]bool) ([]OrphanedPatch, []UnreadablePath) {
	var out []OrphanedPatch
	var failures []UnreadablePath
	var walk func(string)
	walk = func(dir string) {
		dirEntries, err := deps.ReadDir(dir)
		if err != nil {
			failures = append(failures, UnreadablePath{
				Path: dir, Kind: UnreadableOrphanDir, Error: err.Error(),
			})
			return
		}
		sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name() < dirEntries[j].Name() })
		for _, de := range dirEntries {
			full := filepath.Clean(filepath.Join(dir, de.Name()))
			if de.IsDir() {
				walk(full)
				continue
			}
			name := de.Name()
			if !strings.HasSuffix(name, ".patch") && !strings.HasSuffix(name, ".diff") {
				continue
			}
			if !referenced[full] {
				out = append(out, OrphanedPatch{Filename: name, Path: full})
			}
		}
	}
	walk(portDir)
	return out, failures
}
