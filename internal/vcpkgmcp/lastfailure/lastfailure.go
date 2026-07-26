package lastfailure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/discovery"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Deps bounds every ambient input LastFailure reads, mirroring the
// discovery package's determinism seam (see discovery.Deps doc comment):
// nothing here reads os.Getenv directly, so tests fully control it.
type Deps struct {
	FS        FS
	Getenv    func(string) string
	Discovery discovery.Deps
}

// DefaultDeps wires Deps to the real OS.
//
// Getenv is os.Getenv. It previously returned "" unconditionally, which made
// the injection seam silently swallow production behaviour: resolveOverlayChain
// consults VCPKG_OVERLAY_PORTS, so a real invocation that used that variable
// was reported with overlay_chain_none_builtin_ports_only — a positive claim
// that no overlays were in play, made without ever reading the variable that
// says otherwise. The seam exists so TESTS can control the environment, not so
// production reads nothing.
func DefaultDeps() Deps {
	return Deps{
		FS:        DefaultFS(),
		Getenv:    os.Getenv,
		Discovery: discovery.DefaultDeps(),
	}
}

// absoluteRoot validates a root-like parameter. See ReasonRelativeRoot: a
// relative root binds to the hub daemon's working directory, which is not
// the caller's and not the one the recorded vcpkg invocation ran in, so no
// answer derived from it can be trusted.
//
// filepath.IsAbs is exactly the right predicate on Windows: it rejects
// drive-relative forms like `\vcpkg` and `C:vcpkg`, which are genuinely
// ambiguous (they depend on the process's current drive and per-drive
// working directory) rather than merely relative.
func absoluteRoot(path string) bool {
	return filepath.IsAbs(path)
}

// vcpkgConfiguration is the minimal shape this package reads from a
// vcpkg-configuration.json — only the field last_failure's overlay-chain
// fallback needs. Unknown fields are ignored by encoding/json by default.
type vcpkgConfiguration struct {
	OverlayPorts []string `json:"overlay-ports"`
}

// unknownResult builds a Status=unknown Result with the given reason,
// preserving whatever context/log evidence was already accumulated.
func unknownResult(reason Reason, ev evidence.Evidence, notes []Note, sources []ContextSource) Result {
	return Result{
		Status:        evidence.StatusUnknown,
		Reason:        reason,
		ContextSource: dedupSources(sources),
		Notes:         notes,
		Evidence:      ev,
	}
}

func dedupSources(in []ContextSource) []ContextSource {
	seen := map[ContextSource]bool{}
	var out []ContextSource
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// LastFailure implements vcpkg_last_failure. See package doc for the
// buildtrees-primary / wrapper-optional layering this follows.
func LastFailure(args Args, deps Deps) Result {
	var ev evidence.Evidence
	var notes []Note
	var sources []ContextSource

	// --- Step 1: optional wrapper enrichment -------------------------------
	var wrapperInfo WrapperInfo
	wrapperOK := false
	if strings.TrimSpace(args.BuildFailedLog) != "" {
		if data, err := deps.FS.ReadFile(args.BuildFailedLog); err == nil {
			wrapperInfo, wrapperOK, _ = ParseWrapperContent(data)
			if wrapperOK {
				notes = append(notes, NoteWrapperUsedForContext)
				sources = append(sources, SourceWrapperSummary)
				ev.AddPath(args.BuildFailedLog)
			} else {
				notes = append(notes, NoteWrapperMalformed)
			}
		} else {
			notes = append(notes, NoteWrapperMalformed)
		}
	} else {
		notes = append(notes, NoteWrapperAbsent)
	}

	// --- Step 2: determine port ---------------------------------------------
	port := strings.TrimSpace(args.Port)
	triplet := strings.TrimSpace(args.Triplet)
	if port == "" {
		switch {
		case wrapperOK && len(wrapperInfo.FailedPorts) == 1:
			p, t := PortNameFromEntry(wrapperInfo.FailedPorts[0])
			port = p
			if triplet == "" {
				triplet = t
			}
			notes = append(notes, NotePortAutoSelectedFromWrapper)
		case wrapperOK && len(wrapperInfo.FailedPorts) > 1:
			// Ambiguous: report every candidate, never silently pick one.
			// The failed_ports list itself is the payload; FailedTarget
			// carries the joined summary so a caller sees the full set
			// without a second call.
			res := unknownResult(ReasonMultipleFailedPortsAmbiguous, ev, notes, sources)
			res.FailedTarget = strings.Join(wrapperInfo.FailedPorts, ", ")
			return res
		default:
			return unknownResult(ReasonPortNotSpecified, ev, notes, sources)
		}
	} else if triplet == "" && wrapperOK && wrapperInfo.Triplet != "" {
		triplet = wrapperInfo.Triplet
	}

	// --- Step 2a: the port name must be ONE legal port-name segment ----------
	// Validated here, AFTER auto-selection, so a hostile or malformed wrapper
	// file cannot smuggle a traversal segment in through failed_ports either.
	if !portNameRE.MatchString(port) {
		res := unknownResult(ReasonInvalidPortName, ev, notes, sources)
		res.FailedTarget = port
		return res
	}

	// --- Step 2b: wrapper's own failed_ports list can positively confirm
	// this port did NOT fail in that run — but ONLY when that list is proven
	// EXHAUSTIVE. An incomplete list's silence about a port is not evidence;
	// treating it as such tells the operator to stop looking at a port that
	// did fail. When completeness cannot be established the tool says so and
	// falls through to buildtree evidence, which is strictly better than
	// either guessing or refusing to answer.
	if wrapperOK && len(wrapperInfo.FailedPorts) > 0 && !portListedAsFailed(wrapperInfo.FailedPorts, port, triplet) {
		if wrapperInfo.FailedPortsListIsComplete() {
			return Result{
				Status:        evidence.StatusOK,
				FailedTarget:  port,
				ContextSource: dedupSources(append(sources, SourceWrapperSummary)),
				Notes:         append(notes, NoteWrapperConfirmsNoFailure),
				Evidence:      ev,
			}
		}
		notes = append(notes, NoteWrapperFailedPortsIncomplete)
	}

	// --- Step 3: determine buildtrees root -----------------------------------
	buildtreesRoot := strings.TrimSpace(args.BuildtreesRoot)
	if buildtreesRoot == "" && wrapperOK && wrapperInfo.BuildtreesRoot != "" {
		// Also covered by the absolute-root gate below: vcpkg resolves a
		// relative --x-buildtrees-root against the shell that ran it, whose
		// working directory this tool has no way to know.
		buildtreesRoot = wrapperInfo.BuildtreesRoot
	}
	if buildtreesRoot == "" {
		// An explicit but relative root would be resolved against the daemon's
		// working directory by discovery too, so it is rejected before it can
		// produce a confident answer about the wrong installation.
		if strings.TrimSpace(args.Root) != "" && !absoluteRoot(strings.TrimSpace(args.Root)) {
			res := unknownResult(ReasonRelativeRoot, ev, notes, sources)
			res.FailedTarget = port
			return res
		}
		discRes := discovery.DiscoverRoot(args.Root, deps.Discovery)
		if discRes.Status != evidence.StatusOK {
			ev.Paths = append(ev.Paths, discRes.Evidence.Paths...)
			res := unknownResult(ReasonRootNotSpecified, ev, notes, sources)
			return res
		}
		buildtreesRoot = filepath.Join(discRes.Root, "buildtrees")
	}
	if !absoluteRoot(buildtreesRoot) {
		res := unknownResult(ReasonRelativeRoot, ev, notes, sources)
		res.FailedTarget = port
		return res
	}
	sources = append(sources, SourceBuildtrees)
	ev.AddPath(buildtreesRoot)

	// --- Step 4: buildtrees root must exist (else --clean-buildtrees-after-build
	// almost certainly removed it) -------------------------------------------
	switch p, _ := probeDir(deps.FS, buildtreesRoot); p {
	case evidence.PresenceAbsent:
		res := unknownResult(ReasonBuildtreesCleaned, ev, notes, sources)
		res.FailedTarget = port
		if wrapperOK && wrapperInfo.ExitCode != nil {
			res.ExitCode = wrapperInfo.ExitCode
		}
		chain, notesOut := resolveOverlayChain(args, wrapperInfo, wrapperOK, deps)
		res.OverlayChain = chain
		res.Notes = append(res.Notes, notesOut...)
		return res
	case evidence.PresenceUnreadable:
		res := unknownResult(ReasonBuildtreesRootUnreadable, ev, notes, sources)
		res.FailedTarget = port
		return res
	}

	// --- Step 5: port directory must exist -----------------------------------
	portDir, perr := portDirWithin(buildtreesRoot, port)
	if perr != nil {
		res := unknownResult(ReasonInvalidPortName, ev, notes, sources)
		res.FailedTarget = port
		return res
	}
	switch p, _ := probeDir(deps.FS, portDir); p {
	case evidence.PresenceAbsent:
		res := unknownResult(ReasonPortDirNotFound, ev, notes, sources)
		res.FailedTarget = port
		return res
	case evidence.PresenceUnreadable:
		res := unknownResult(ReasonPortDirUnreadable, ev, notes, sources)
		res.FailedTarget = port
		return res
	}
	ev.AddPath(portDir)

	// --- Step 6: resolve triplet if still unknown ----------------------------
	if triplet == "" {
		t, candidates, ok := detectTripletFromPortDir(deps.FS, portDir)
		switch {
		case ok:
			triplet = t
			notes = append(notes, NoteTripletAutoSelectedFromDir)
		case len(candidates) > 1:
			res := unknownResult(ReasonTripletAmbiguous, ev, notes, sources)
			res.FailedTarget = port
			return res
		}
		// else: still unknown; classifyPortDir below will simply find no
		// triplet-qualified config/install files (extract-phase files, which
		// carry no triplet token, still work).
	}

	// --- Step 7: classify phase logs -----------------------------------------
	phases, otherLogPaths, configureLogYAMLPaths, stdoutNarration, err := classifyPortDir(deps.FS, portDir, triplet)
	if err != nil {
		res := unknownResult(ReasonNoPhaseLogsFound, ev, notes, sources)
		res.FailedTarget = port
		return res
	}
	for _, p := range otherLogPaths {
		ev.AddPath(p)
	}
	if len(phases) == 0 {
		res := unknownResult(ReasonNoPhaseLogsFound, ev, notes, sources)
		res.FailedTarget = port
		res.LogPaths = append(res.LogPaths, otherLogPaths...)
		return res
	}

	// --- Step 8: scan phases in build order --------------------------------
	// Selection is driven by ERROR-severity diagnostics only. A warning-only
	// log establishes nothing (see ContainsFailureDiagnostic), and letting a
	// later phase's warnings override an earlier phase's real error would
	// misreport the phase as well as the verdict. Warning-only matches are
	// still retained, as the payload of unknown(no_failure_diagnostic).
	//
	// A "FAILED:" line ANYWHERE can be the tail of a user-interrupted build
	// (scout-pass trap 2) rather than a genuine defect — checked across
	// every phase log read, never assumed absent.
	//
	// An UNREADABLE log is tracked rather than skipped: it can hold a later
	// phase's error, the only error in the port, or an interrupt marker, so
	// a confident verdict cannot be issued while one is unread (F9).
	// vcpkg's own step order. PhaseBuild sits between config and install: a
	// non-ninja port runs a separate build step (build-<triplet>-<cfg>-*.log)
	// before installing, whereas a ninja port compiles AND installs inside
	// the install step (which is why an install-log compiler error is
	// re-reported as `build` further down).
	phaseOrder := []Phase{PhaseExtract, PhasePatch, PhaseConfig, PhaseBuild, PhaseInstall}
	var errPhase, anyPhase Phase
	var errDiags, anyDiags []Diagnostic
	var allLogPaths []string
	var errOutLog, errErrLog string
	var anyOutLog, anyErrLog string
	var unreadableLogs []string
	interrupted := false

	for _, ph := range phaseOrder {
		var thisPhaseDiags []Diagnostic
		var thisOut, thisErr string
		for _, pf := range phases {
			if pf.Phase != ph {
				continue
			}
			allLogPaths = append(allLogPaths, pf.Path)
			ev.AddPath(pf.Path)
			data, rerr := deps.FS.ReadFile(pf.Path)
			if rerr != nil {
				unreadableLogs = append(unreadableLogs, pf.Path)
				continue
			}
			if DetectInterrupted(data) {
				interrupted = true
			}
			d := ScanDiagnostics(data)
			thisPhaseDiags = append(thisPhaseDiags, d...)
			if pf.Stream == "out" {
				thisOut = pf.Path
			} else {
				thisErr = pf.Path
			}
		}
		if len(thisPhaseDiags) == 0 {
			continue
		}
		anyPhase, anyDiags = ph, thisPhaseDiags
		anyOutLog, anyErrLog = thisOut, thisErr
		if ContainsFailureDiagnostic(thisPhaseDiags) {
			errPhase, errDiags = ph, thisPhaseDiags
			errOutLog, errErrLog = thisOut, thisErr
		}
	}

	// Bonus/fallback: the top-level per-port narration log, only consulted
	// when no primary phase log established a failure.
	if stdoutNarration != "" {
		allLogPaths = append(allLogPaths, stdoutNarration)
		ev.AddPath(stdoutNarration)
	}
	if len(errDiags) == 0 && stdoutNarration != "" {
		data, rerr := deps.FS.ReadFile(stdoutNarration)
		if rerr != nil {
			unreadableLogs = append(unreadableLogs, stdoutNarration)
		} else {
			if DetectInterrupted(data) {
				interrupted = true
			}
			if d := ScanDiagnostics(data); len(d) > 0 {
				if len(anyDiags) == 0 {
					anyPhase, anyDiags, anyOutLog = PhaseInstall, d, stdoutNarration
				}
				if ContainsFailureDiagnostic(d) {
					errPhase, errDiags, errOutLog = PhaseInstall, d, stdoutNarration
					errErrLog = ""
				}
			}
		}
	}

	// Last resort: a CMakeConfigureLog.yaml.log capability-probe dump. A
	// try_compile that fails here is the NORMAL mechanism of CMake feature
	// detection, so an error recovered ONLY from this source does not
	// establish that the port failed — it is reported as
	// unknown(capability_probe_only) with the diagnostics attached, never as
	// a confident `failed` carrying a mere advisory note (F7).
	usedCapabilityProbeLog := false
	if len(errDiags) == 0 {
		for _, p := range configureLogYAMLPaths {
			data, rerr := deps.FS.ReadFile(p)
			if rerr != nil {
				unreadableLogs = append(unreadableLogs, p)
				continue
			}
			if DetectInterrupted(data) {
				interrupted = true
			}
			d := ScanDiagnostics(data)
			if !ContainsFailureDiagnostic(d) {
				continue
			}
			errPhase, errDiags, errOutLog = PhaseConfig, d, p
			errErrLog = ""
			if len(anyDiags) == 0 {
				anyPhase, anyDiags, anyOutLog = PhaseConfig, d, p
			}
			usedCapabilityProbeLog = true
			break
		}
	}

	// Resolve which set the answer is built from: an error-bearing selection
	// when one exists, otherwise the warning-only evidence.
	chosenPhase, chosenDiags := errPhase, errDiags
	chosenOutLog, chosenErrLog := errOutLog, errErrLog
	if len(chosenDiags) == 0 {
		chosenPhase, chosenDiags = anyPhase, anyDiags
		chosenOutLog, chosenErrLog = anyOutLog, anyErrLog
	}

	overlayChain, overlayNotes := resolveOverlayChain(args, wrapperInfo, wrapperOK, deps)
	notes = append(notes, overlayNotes...)
	if usedCapabilityProbeLog {
		notes = append(notes, NoteDiagnosticFromCapabilityProbeLog)
	}

	exactCommand := extractExactCommand(deps.FS, chosenPhase, chosenOutLog, chosenErrLog, wrapperOK, wrapperInfo)

	base := Result{
		FailedTarget:  port,
		ExactCommand:  exactCommand,
		LogPaths:      dedupStrings(append(allLogPaths, otherLogPaths...)),
		OverlayChain:  overlayChain,
		ContextSource: dedupSources(sources),
		Notes:         notes,
		Evidence:      ev,
	}
	if wrapperOK && wrapperInfo.ExitCode != nil {
		base.ExitCode = wrapperInfo.ExitCode
	}

	// Diagnostics found are always returned, whatever the verdict — they are
	// the evidence the caller judges, and withholding them on an `unknown`
	// would force a re-read of the same logs.
	base.Diagnostics = chosenDiags

	reportedPhase := chosenPhase
	if reportedPhase == PhaseInstall {
		// vcpkg's own ninja invocation compiles AND installs in one "install"
		// step; a compiler/linker-shaped diagnostic found there means the
		// build (not the file-copy install step) is what actually failed.
		reportedPhase = PhaseBuild
	}

	// Verdict precedence, strongest positive evidence first.
	switch {
	case interrupted:
		// A ninja user-interrupt overrides any "FAILED:"-shaped match found
		// alongside it — the build was stopped, not broken; reporting it as
		// a defect would misdirect the operator (scout-pass trap 2). This
		// outranks the unreadable-log reason below because it is a positive
		// finding from evidence that WAS read.
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonBuildInterrupted

	case len(unreadableLogs) > 0:
		// F9: at least one relevant phase log could not be read. Any of the
		// verdicts below could be changed by its contents, so none of them
		// may be issued. The readable evidence is still returned.
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonPhaseLogUnreadable
		base.Phase = reportedPhase

	case usedCapabilityProbeLog:
		// F7: the only error came from a try_compile dump.
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonCapabilityProbeOnly
		base.Phase = reportedPhase

	case len(chosenDiags) == 0:
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonNoDiagnosticFound

	case !ContainsFailureDiagnostic(chosenDiags):
		// F6: recognized diagnostics, but every one is a warning or a note.
		// That is the normal state of a SUCCESSFUL C++ build.
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonNoFailureDiagnostic
		base.Phase = reportedPhase

	default:
		base.Status = evidence.StatusFailed
		base.Phase = reportedPhase
	}
	return base
}

// portListedAsFailed reports whether port (optionally triplet-qualified)
// appears in a wrapper's failed_ports list.
func portListedAsFailed(failedPorts []string, port, triplet string) bool {
	for _, entry := range failedPorts {
		p, t := PortNameFromEntry(entry)
		if p != port {
			continue
		}
		if triplet == "" || t == "" || t == triplet {
			return true
		}
	}
	return false
}

// resolveOverlayChain implements the corrected precedence: the wrapper file
// (when present and it recovered an overlay chain) is the ACTUAL invocation
// and is trusted above every other source; absent that, explicit param ->
// VCPKG_OVERLAY_PORTS -> vcpkg-configuration.json overlay-ports -> none.
func resolveOverlayChain(args Args, wrapperInfo WrapperInfo, wrapperOK bool, deps Deps) ([]string, []Note) {
	if wrapperOK && len(wrapperInfo.OverlayPorts) > 0 {
		return wrapperInfo.OverlayPorts, []Note{NoteOverlayChainFromWrapper}
	}
	if len(args.Overlays) > 0 {
		return args.Overlays, []Note{NoteOverlayChainFromParam}
	}
	if deps.Getenv != nil {
		if val := deps.Getenv("VCPKG_OVERLAY_PORTS"); strings.TrimSpace(val) != "" {
			return filepath.SplitList(val), []Note{NoteOverlayChainFromEnv}
		}
	}
	if args.Root != "" {
		if data, err := deps.FS.ReadFile(filepath.Join(args.Root, "vcpkg-configuration.json")); err == nil {
			var cfg vcpkgConfiguration
			if json.Unmarshal(data, &cfg) == nil && len(cfg.OverlayPorts) > 0 {
				return cfg.OverlayPorts, []Note{NoteOverlayChainFromParam}
			}
		}
	}
	return nil, []Note{NoteOverlayChainNone}
}

// extractExactCommand recovers the most specific "exact failing command"
// available: the phase's own invocation line when a phase log was read,
// falling back to the wrapper's whole-batch command when nothing more
// specific is available.
func extractExactCommand(fsys FS, phase Phase, outLog, errLog string, wrapperOK bool, wrapperInfo WrapperInfo) string {
	if outLog != "" {
		if data, err := fsys.ReadFile(outLog); err == nil {
			if phase == PhaseInstall || phase == PhaseBuild {
				if cmd, ok := findRunBuildCommandLine(data); ok {
					return cmd
				}
			}
			if line, ok := firstNonEmptyLine(data); ok {
				return line
			}
		}
	}
	if wrapperOK && wrapperInfo.Command != "" {
		return wrapperInfo.Command
	}
	return ""
}

func findRunBuildCommandLine(data []byte) (string, bool) {
	const marker = "Run Build Command(s): "
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if idx := strings.Index(trimmed, marker); idx >= 0 {
			return strings.TrimSpace(trimmed[idx+len(marker):]), true
		}
	}
	return "", false
}

func firstNonEmptyLine(data []byte) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if trimmed != "" {
			return trimmed, true
		}
	}
	return "", false
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
