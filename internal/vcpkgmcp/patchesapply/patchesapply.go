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
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
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
	// <root>/triplets/community. Must be absolute; a relative value returns
	// failed(relative_vcpkg_root) before filesystem access rather than being
	// bound to the daemon's working directory.
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
	// MaxOverlayTripletRoots bounds the caller-owned root chain before any
	// filesystem operation. The MCP schema imports this owner constant.
	MaxOverlayTripletRoots   = 64
	MaxVarOverrides          = 64
	MaxVarOverrideNameBytes  = 256
	MaxVarOverrideValueBytes = int(MaxPortfileBytes)
	MaxVarOverrideTotalBytes = int(MaxPortfileBytes)
	// ReasonEmptyPortDir: PortDir was empty/whitespace-only. Failed (bad
	// caller input, not an environment condition).
	ReasonEmptyPortDir Reason = "empty_port_dir"
	// ReasonEmptyTriplet: Triplet was empty/whitespace-only. Failed.
	ReasonEmptyTriplet Reason = "empty_triplet"
	// ReasonRelativePortDir: PortDir is not absolute. Failed (bad caller
	// input), and refused BEFORE the path is used.
	//
	// The schema has always required an absolute path, but the value went
	// straight to Stat, which resolves a relative path against the process
	// working directory — the hub daemon's, not the caller's. That silently
	// answers about a DIFFERENT port directory than the one asked about, and
	// on a daemon whose cwd the caller cannot see it is not even diagnosable.
	// Same posture, and the same reason spelling, as lastfailure's
	// relative_root gate for root/buildtrees_root.
	ReasonRelativePortDir            Reason = "relative_port_dir"
	ReasonTooManyOverlayTripletRoots Reason = "too_many_overlay_triplet_roots"
	ReasonRelativeOverlayTripletRoot Reason = "relative_overlay_triplet_root"
	ReasonRelativeVcpkgRoot          Reason = "relative_vcpkg_root"
	ReasonVarOverridesLimitExceeded  Reason = "var_overrides_limit_exceeded"
	// ReasonPortDirMissing: the OS positively reported that PortDir does not
	// exist, or that it exists but is not a directory. A VERIFIED negative —
	// never a probe that merely failed (see ReasonPortDirUnreadable).
	ReasonPortDirMissing Reason = "port_dir_missing"
	// ReasonPortDirUnreadable: the probe of PortDir itself failed (permission
	// denied, sharing violation, disconnected network path), so whether the
	// directory exists is UNKNOWN.
	//
	// It is distinct from ReasonPortDirMissing because the remedy is different
	// — fix access, versus correct the path — and because collapsing the two
	// tells an operator their port directory is gone when it is merely locked.
	// This is the evidence.Presence distinction the shared owner already
	// models; every other probe in this package routes through it.
	ReasonPortDirUnreadable Reason = "port_dir_unreadable"
	// ReasonPortfileUnreadable: PortDir exists but portfile.cmake could not
	// be read (missing, permission denied, ...).
	ReasonPortfileUnreadable Reason = "portfile_unreadable"
	// ReasonNoPatchesDeclared: the portfile never references a PATCHES
	// keyword-arg at all (the netgen shape) — every physical .patch/.diff
	// file in the port directory is therefore orphaned by construction.
	ReasonNoPatchesDeclared Reason = "no_patches_declared"
	// ReasonPatchesExprUnparsable: the portfile's statement structure is broken
	// (unbalanced parentheses before EOF), or a PATCHES value expression
	// (including a malformed supported variable reference) cannot be resolved
	// safely, so no extracted entry can be trusted. A single
	// unparsable if()-condition INSIDE an otherwise well-formed file does NOT
	// produce this — that guard degrades to the per-entry Undecidable bucket
	// instead (see condition.go).
	ReasonPatchesExprUnparsable Reason = "patches_expression_unparsable"
	// ReasonPortfileSizeLimitExceeded: portfile.cmake exceeded the bounded
	// streaming read limit, so no prefix is parsed as a complete portfile.
	ReasonPortfileSizeLimitExceeded Reason = "portfile_size_limit_exceeded"
	// ReasonPatchesDeferredCommandBody: a PATCHES flow crosses a deferred
	// function or macro declaration or invocation boundary. Static analysis
	// never executes declaration bodies for their side effects.
	ReasonPatchesDeferredCommandBody Reason = "patches_deferred_command_body"
	// ReasonPatchesExecutionUncertain: a return() may or may not execute, so
	// statements after it cannot be classified by a static linear walk.
	ReasonPatchesExecutionUncertain Reason = "patches_execution_uncertain"
	// ReasonPatchDeclarationLimitExceeded: PATCHES expansion exceeded the
	// bounded declaration count or retained-byte budget. No patch path was
	// probed from a partial declaration inventory.
	ReasonPatchDeclarationLimitExceeded Reason = "patch_declaration_limit_exceeded"

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
	// ReasonTripletFileSizeLimitExceeded: a selected triplet exceeded the
	// complete-CMake-input limit, so no partial facts were parsed.
	ReasonTripletFileSizeLimitExceeded Reason = "triplet_file_size_limit_exceeded"
	// ReasonPatchPathUnreadable: at least one declared patch path could not
	// be probed. Such a path is NOT in Missing — "missing" is a real defect
	// report, and a permission error is not evidence that a file is absent.
	ReasonPatchPathUnreadable Reason = "patch_path_unreadable"
	// ReasonOrphanScanIncomplete: a directory under the port dir could not be
	// completely classified because a referenced patch identity was unresolved,
	// a directory was unreadable, a traversal budget was reached, or the request
	// was cancelled. No partial orphan inventory may report an overall ok verdict.
	ReasonOrphanScanIncomplete Reason = "orphan_scan_incomplete"
)

// UnreadableKind names which question a probe failed to answer. Closed enum.
type UnreadableKind string

const (
	UnreadablePatchPath   UnreadableKind = "patch_path"
	UnreadableOrphanDir   UnreadableKind = "orphan_scan_dir"
	UnreadableTripletFile UnreadableKind = "triplet_file"
	// UnreadablePortDir: the port directory itself could not be probed, so
	// nothing below it could be inspected either.
	UnreadablePortDir UnreadableKind = "port_dir"
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

// OrphanScanStopCause is the typed boundary that prevented a complete orphan
// inventory: either reference identity was unresolved or traversal stopped.
// It remains populated when another evidence-integrity reason has higher
// verdict precedence, so incomplete coverage is never hidden.
type OrphanScanStopCause string

const (
	OrphanScanStopDirectoryUnreadable     OrphanScanStopCause = "directory_unreadable"
	OrphanScanStopEntryLimit              OrphanScanStopCause = "entry_limit_exceeded"
	OrphanScanStopDirectoryLimit          OrphanScanStopCause = "directory_limit_exceeded"
	OrphanScanStopDepthLimit              OrphanScanStopCause = "depth_limit_exceeded"
	OrphanScanStopCancelled               OrphanScanStopCause = "cancelled"
	OrphanScanStopUnresolvedPatchIdentity OrphanScanStopCause = "unresolved_patch_identity"
)

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
	// OrphanScanStopCause explains why an orphan inventory is incomplete. It
	// is separate from unreadable paths: resource limits and cancellation are
	// not filesystem unreadability. It stays visible if an earlier reason wins
	// the established verdict precedence.
	OrphanScanStopCause OrphanScanStopCause `json:"orphan_scan_stop_cause,omitempty"`

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
	Stat    func(path string) (os.FileInfo, error)
	Open    func(path string) (io.ReadCloser, error)
	OpenDir func(path string) (boundedio.DirReader, error)
}

// DefaultDeps wires Deps to the real OS. Production callers use this; tests
// build their own Deps (or just point Args.PortDir at a t.TempDir() fixture
// and use DefaultDeps, which is simplest — nothing here is mocked out at
// the OS boundary, only the environment/inputs are fixture-controlled).
func DefaultDeps() Deps {
	return Deps{
		Stat: os.Stat,
		Open: func(path string) (io.ReadCloser, error) {
			return os.Open(path)
		},
		OpenDir: func(path string) (boundedio.DirReader, error) {
			return os.Open(path)
		},
	}
}

// ApplyOrder executes vcpkg_patches_apply against the real filesystem.
func ApplyOrder(args Args) Result {
	return ApplyOrderContext(context.Background(), args)
}

// ApplyOrderContext executes vcpkg_patches_apply against the real filesystem
// and observes cancellation while streaming the portfile and walking orphans.
func ApplyOrderContext(ctx context.Context, args Args) Result {
	return applyOrderContext(ctx, args, DefaultDeps())
}

func applyOrder(args Args, deps Deps) Result {
	return applyOrderContext(context.Background(), args, deps)
}

const (
	// MaxPortfileBytes is the package's complete-portfile admission limit. The
	// next byte is read only as an overflow sentinel and is never parsed.
	MaxPortfileBytes int64 = publicresult.MaxEncodedBytes
	// MaxTripletFileBytes uses the same complete-CMake-input policy as a
	// portfile. This alias keeps that policy with its semantic owner.
	MaxTripletFileBytes          = MaxPortfileBytes
	portfileReadBatchBytes int64 = 32 << 10
)

type boundedDeps struct{ deps Deps }

func (d boundedDeps) Open(path string) (io.ReadCloser, error) {
	return d.deps.Open(path)
}

func (d boundedDeps) Stat(path string) (os.FileInfo, error) { return d.deps.Stat(path) }

func (d boundedDeps) OpenDir(path string) (boundedio.DirReader, error) {
	return d.deps.OpenDir(path)
}

func applyOrderContext(ctx context.Context, args Args, deps Deps) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args.OverlayTriplets) > MaxOverlayTripletRoots {
		return Result{Status: evidence.StatusFailed, Reason: ReasonTooManyOverlayTripletRoots}
	}
	if !varOverridesWithinLimits(args.VarOverrides) {
		return Result{Status: evidence.StatusFailed, Reason: ReasonVarOverridesLimitExceeded}
	}
	var ev evidence.Evidence
	// Declared up front because the port-dir probe below can already produce
	// an entry: an unreadable port dir is the FIRST question the filesystem
	// can decline to answer, and it must be listed like every other.
	var unreadable []UnreadablePath

	portDir := strings.TrimSpace(args.PortDir)
	triplet := strings.TrimSpace(args.Triplet)

	if portDir == "" {
		return Result{Status: evidence.StatusFailed, Reason: ReasonEmptyPortDir, Evidence: ev}
	}
	if triplet == "" {
		return Result{Status: evidence.StatusFailed, Reason: ReasonEmptyTriplet, Evidence: ev}
	}
	// Refused BEFORE the path is used for anything: a relative port_dir would
	// bind to the hub daemon's working directory and silently answer about a
	// different port. Never call filepath.Abs to "fix" it — that IS the bind.
	if !filepath.IsAbs(portDir) {
		return Result{
			Status: evidence.StatusFailed, Reason: ReasonRelativePortDir,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
		}
	}
	for _, root := range args.OverlayTriplets {
		root = strings.TrimSpace(root)
		if root != "" && !filepath.IsAbs(root) {
			return Result{Status: evidence.StatusFailed, Reason: ReasonRelativeOverlayTripletRoot, Triplet: triplet, PortDir: portDir, Evidence: ev}
		}
	}
	if root := strings.TrimSpace(args.VcpkgRoot); root != "" && !filepath.IsAbs(root) {
		return Result{Status: evidence.StatusFailed, Reason: ReasonRelativeVcpkgRoot, Triplet: triplet, PortDir: portDir, Evidence: ev}
	}

	ev.AddPath(portDir)
	// Tri-state, via the shared owner: an access-denied Stat is not evidence
	// that the port directory is absent, and reporting it as missing sends the
	// operator to fix the wrong thing.
	portDirPresence, portDirErr := evidence.ProbeDir(evidence.StatFunc(deps.Stat), portDir)
	if portDirPresence != evidence.PresenceExists {
		reason := ReasonPortDirMissing
		if portDirPresence == evidence.PresenceUnreadable {
			reason = ReasonPortDirUnreadable
			errText := ""
			if portDirErr != nil {
				errText = portDirErr.Error()
			}
			unreadable = append(unreadable, UnreadablePath{
				Path: portDir, Kind: UnreadablePortDir, Error: errText,
			})
		}
		return Result{
			Status: evidence.StatusUnknown, Reason: reason,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
			Unreadable: unreadable,
		}
	}

	portfilePath := filepath.Join(portDir, "portfile.cmake")
	ev.AddPath(portfilePath)
	portfile, err := boundedio.ReadFile(ctx, boundedDeps{deps: deps}, portfilePath, MaxPortfileBytes, portfileReadBatchBytes)
	if err != nil {
		return Result{
			Status: evidence.StatusUnknown, Reason: ReasonPortfileUnreadable,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
		}
	}
	ev.AddCommand("read " + portfilePath)
	if portfile.Limited {
		return Result{
			Status: evidence.StatusUnknown, Reason: ReasonPortfileSizeLimitExceeded,
			Triplet: triplet, PortDir: portDir, Evidence: ev,
		}
	}

	portName := strings.TrimSpace(args.PortName)
	if portName == "" {
		portName = filepath.Base(portDir)
	}
	vcpkgRoot := strings.TrimSpace(args.VcpkgRoot)

	// Triplet variables come from an ACTUAL triplet file, never from the
	// triplet name — see triplet.go. No file found means every triplet
	// variable stays unresolved, which surfaces as undecidable guards rather
	// than as invented ON/OFF/static/dynamic values.
	var tripletFacts map[string]string
	tripletFile, tripletPresence, tripletErr := resolveTripletFile(deps, triplet, args.OverlayTriplets, vcpkgRoot)
	switch tripletPresence {
	case evidence.PresenceExists:
		ev.AddPath(tripletFile)
		tripletRaw, terr := boundedio.ReadFile(ctx, boundedDeps{deps: deps}, tripletFile, MaxTripletFileBytes, portfileReadBatchBytes)
		if terr != nil {
			unreadable = append(unreadable, UnreadablePath{
				Path: tripletFile, Kind: UnreadableTripletFile, Error: terr.Error(),
			})
		} else if tripletRaw.Limited {
			return Result{
				Status: evidence.StatusUnknown, Reason: ReasonTripletFileSizeLimitExceeded,
				Triplet: triplet, PortDir: portDir, TripletFile: tripletFile, Evidence: ev,
			}
		} else {
			ev.AddCommand("read " + tripletFile)
			tripletFacts = parseTripletFacts(string(tripletRaw.Data), portDir, portName, vcpkgRoot)
		}
	case evidence.PresenceUnreadable:
		ev.AddPath(tripletFile)
		errText := ""
		if tripletErr != nil {
			errText = tripletErr.Error()
		}
		unreadable = append(unreadable, UnreadablePath{
			Path: tripletFile, Kind: UnreadableTripletFile, Error: errText,
		})
	}

	// Every post-triplet path starts from this one base result. In particular,
	// structural parser stops retain the located triplet identity, preobserved
	// triplet evidence, and a higher-priority unreadable-triplet reason.
	res := Result{
		Triplet:     triplet,
		PortDir:     portDir,
		TripletFile: tripletFile,
		Evidence:    ev,
		Unreadable:  unreadable,
	}
	if tripletPresence == evidence.PresenceAbsent {
		res.TripletFile = ""
	}

	env := newVarEnv(portDir, portName, vcpkgRoot, args.VarOverrides, tripletFacts)
	entries, sawPatches, structural := walkPortfile(string(portfile.Data), env)
	if structural != parserStructuralNone {
		return finalizePostTripletResult(res, ev, unreadable, sawPatches, structural)
	}
	referenced := map[string]bool{}
	orphanIdentityIncomplete := false
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
			orphanIdentityIncomplete = true
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
	if orphanIdentityIncomplete {
		res.OrphanScanStopCause = OrphanScanStopUnresolvedPatchIdentity
		return finalizePostTripletResult(res, ev, unreadable, sawPatches, parserStructuralNone)
	}

	orphans, orphanFailures, orphanStop := findOrphans(ctx, deps, portDir, referenced)
	res.Orphaned = orphans
	unreadable = append(unreadable, orphanFailures...)
	res.OrphanScanStopCause = orphanStop
	return finalizePostTripletResult(res, ev, unreadable, sawPatches, parserStructuralNone)
}

func varOverridesWithinLimits(overrides map[string]string) bool {
	if len(overrides) > MaxVarOverrides {
		return false
	}
	total := 0
	for name, value := range overrides {
		if len(name) > MaxVarOverrideNameBytes || len(value) > MaxVarOverrideValueBytes {
			return false
		}
		cost := len(name) + len(value)
		if cost > MaxVarOverrideTotalBytes-total {
			return false
		}
		total += cost
	}
	return true
}

// finalizePostTripletResult is the sole post-triplet result finalizer. It
// completes evidence fields and owns verdict precedence for normal and
// parser-stop paths alike.
func finalizePostTripletResult(res Result, ev evidence.Evidence, unreadable []UnreadablePath, sawPatches bool, structural parserStructuralSignal) Result {
	res.Evidence = ev
	res.Unreadable = unreadable
	switch {
	case hasUnreadableKind(unreadable, UnreadableTripletFile):
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonTripletFileUnreadable
	case hasUnreadableKind(unreadable, UnreadablePatchPath):
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPatchPathUnreadable
	case res.OrphanScanStopCause != "":
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonOrphanScanIncomplete
	case structural == parserStructuralExpressionUnparsable:
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPatchesExprUnparsable
	case structural == parserStructuralDeferredBody:
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPatchesDeferredCommandBody
	case structural == parserStructuralExecutionUncertain:
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPatchesExecutionUncertain
	case structural == parserStructuralDeclarationLimit:
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPatchDeclarationLimitExceeded
	case !sawPatches:
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNoPatchesDeclared
	default:
		res.Status = evidence.StatusOK
		res.Reason = ""
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

const (
	// MaxOrphanScanEntries bounds all directory entries admitted by a call.
	// A per-directory plus-one sentinel proves an overflowing directory without
	// returning an operating-system-order prefix as evidence.
	MaxOrphanScanEntries = 4096
	// MaxOrphanScanDirectories includes the port directory itself.
	MaxOrphanScanDirectories = 512
	// MaxOrphanScanDepth counts edges below the port directory.
	MaxOrphanScanDepth = 32
	// OrphanScanReadBatchEntries bounds one filesystem ReadDir request.
	OrphanScanReadBatchEntries = 128
)

// findOrphans recursively scans portDir for .patch/.diff files whose cleaned
// absolute path never appeared in referenced. Every incomplete traversal
// returns one typed stop cause, so no prefix can retain an ok verdict.
func findOrphans(ctx context.Context, deps Deps, portDir string, referenced map[string]bool) ([]OrphanedPatch, []UnreadablePath, OrphanScanStopCause) {
	var out []OrphanedPatch
	var failures []UnreadablePath
	entriesRemaining := MaxOrphanScanEntries
	directoriesSeen := 0

	var walk func(dir string, depth int) OrphanScanStopCause
	walk = func(dir string, depth int) OrphanScanStopCause {
		if err := ctx.Err(); err != nil {
			return OrphanScanStopCancelled
		}
		if directoriesSeen == MaxOrphanScanDirectories {
			return OrphanScanStopDirectoryLimit
		}
		directoriesSeen++

		admitted, err := boundedio.ReadDirComplete(ctx, boundedDeps{deps: deps}, dir, entriesRemaining, OrphanScanReadBatchEntries)
		if err != nil {
			if ctx.Err() != nil {
				return OrphanScanStopCancelled
			}
			failures = append(failures, UnreadablePath{
				Path: dir, Kind: UnreadableOrphanDir, Error: err.Error(),
			})
			return OrphanScanStopDirectoryUnreadable
		}
		if admitted.Limited {
			return OrphanScanStopEntryLimit
		}
		entriesRemaining -= len(admitted.Entries)
		for _, de := range admitted.Entries {
			full := filepath.Clean(filepath.Join(dir, de.Name()))
			if de.Type()&os.ModeSymlink != 0 {
				info, statErr := deps.Stat(full)
				if statErr != nil {
					failures = append(failures, UnreadablePath{Path: full, Kind: UnreadableOrphanDir, Error: statErr.Error()})
					return OrphanScanStopDirectoryUnreadable
				}
				if info.IsDir() {
					failures = append(failures, UnreadablePath{Path: full, Kind: UnreadableOrphanDir, Error: "symlink directory is outside the bounded orphan traversal"})
					return OrphanScanStopDirectoryUnreadable
				}
			}
			if de.IsDir() {
				if depth == MaxOrphanScanDepth {
					return OrphanScanStopDepthLimit
				}
				if stop := walk(full, depth+1); stop != "" {
					return stop
				}
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
		return ""
	}

	stop := walk(portDir, 0)
	return out, failures, stop
}
