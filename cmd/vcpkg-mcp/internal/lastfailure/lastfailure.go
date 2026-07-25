package lastfailure

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/discovery"
	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
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
func DefaultDeps() Deps {
	return Deps{
		FS:        DefaultFS(),
		Getenv:    func(string) string { return "" },
		Discovery: discovery.DefaultDeps(),
	}
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
			wrapperInfo, wrapperOK = ParseWrapperContent(data)
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

	// --- Step 2b: wrapper's own failed_ports list can positively confirm
	// this port did NOT fail in that run — real evidence, not a guess.
	if wrapperOK && len(wrapperInfo.FailedPorts) > 0 && !portListedAsFailed(wrapperInfo.FailedPorts, port, triplet) {
		return Result{
			Status:        evidence.StatusOK,
			ContextSource: dedupSources(append(sources, SourceWrapperSummary)),
			Notes:         append(notes, NoteWrapperConfirmsNoFailure),
			Evidence:      ev,
		}
	}

	// --- Step 3: determine buildtrees root -----------------------------------
	buildtreesRoot := strings.TrimSpace(args.BuildtreesRoot)
	if buildtreesRoot == "" && wrapperOK && wrapperInfo.BuildtreesRoot != "" {
		buildtreesRoot = wrapperInfo.BuildtreesRoot
	}
	if buildtreesRoot == "" {
		discRes := discovery.DiscoverRoot(args.Root, deps.Discovery)
		if discRes.Status != evidence.StatusOK {
			ev.Paths = append(ev.Paths, discRes.Evidence.Paths...)
			res := unknownResult(ReasonRootNotSpecified, ev, notes, sources)
			return res
		}
		buildtreesRoot = filepath.Join(discRes.Root, "buildtrees")
	}
	sources = append(sources, SourceBuildtrees)
	ev.AddPath(buildtreesRoot)

	// --- Step 4: buildtrees root must exist (else --clean-buildtrees-after-build
	// almost certainly removed it) -------------------------------------------
	if !dirExists(deps.FS, buildtreesRoot) {
		res := unknownResult(ReasonBuildtreesCleaned, ev, notes, sources)
		res.FailedTarget = port
		if wrapperOK && wrapperInfo.ExitCode != nil {
			res.ExitCode = wrapperInfo.ExitCode
		}
		chain, notesOut := resolveOverlayChain(args, wrapperInfo, wrapperOK, deps)
		res.OverlayChain = chain
		res.Notes = append(res.Notes, notesOut...)
		return res
	}

	// --- Step 5: port directory must exist -----------------------------------
	portDir := filepath.Join(buildtreesRoot, port)
	if !dirExists(deps.FS, portDir) {
		res := unknownResult(ReasonPortDirNotFound, ev, notes, sources)
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

	// --- Step 8: scan phases in build order, last phase with a match wins ---
	// A "FAILED:" line ANYWHERE can be the tail of a user-interrupted build
	// (scout-pass trap 2) rather than a genuine defect — checked across
	// every phase log read, never assumed absent.
	phaseOrder := []Phase{PhaseExtract, PhasePatch, PhaseConfig, PhaseInstall}
	var chosenPhase Phase
	var chosenDiags []Diagnostic
	var allLogPaths []string
	var chosenOutLog, chosenErrLog string
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
		if len(thisPhaseDiags) > 0 {
			chosenPhase = ph
			chosenDiags = thisPhaseDiags
			chosenOutLog, chosenErrLog = thisOut, thisErr
		}
	}

	// Bonus/fallback: the top-level per-port narration log, only consulted
	// when none of the primary phase logs yielded a match.
	if stdoutNarration != "" {
		allLogPaths = append(allLogPaths, stdoutNarration)
		ev.AddPath(stdoutNarration)
	}
	if len(chosenDiags) == 0 && stdoutNarration != "" {
		if data, rerr := deps.FS.ReadFile(stdoutNarration); rerr == nil {
			if DetectInterrupted(data) {
				interrupted = true
			}
			if d := ScanDiagnostics(data); len(d) > 0 {
				chosenPhase = PhaseInstall
				chosenDiags = d
				chosenOutLog = stdoutNarration
			}
		}
	}

	// Second-to-last resort: a CMakeConfigureLog.yaml.log capability-probe
	// dump. A diagnostic recovered ONLY here may describe a try_compile
	// probe rather than the port's real build (scout-pass finding) — the
	// note below flags exactly that caveat so a caller does not over-trust
	// it as strongly as a primary-phase-log diagnostic.
	usedCapabilityProbeLog := false
	if len(chosenDiags) == 0 {
		for _, p := range configureLogYAMLPaths {
			data, rerr := deps.FS.ReadFile(p)
			if rerr != nil {
				continue
			}
			if DetectInterrupted(data) {
				interrupted = true
			}
			if d := ScanDiagnostics(data); len(d) > 0 {
				chosenPhase = PhaseConfig
				chosenDiags = d
				chosenOutLog = p
				usedCapabilityProbeLog = true
				break
			}
		}
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

	if interrupted {
		// A ninja user-interrupt overrides any "FAILED:"-shaped match found
		// alongside it — the build was stopped, not broken; reporting it as
		// a defect would misdirect the operator (scout-pass trap 2).
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonBuildInterrupted
		return base
	}

	if len(chosenDiags) == 0 {
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonNoDiagnosticFound
		return base
	}

	reportedPhase := chosenPhase
	if reportedPhase == PhaseInstall {
		// vcpkg's own ninja invocation compiles AND installs in one "install"
		// step; a compiler/linker-shaped diagnostic found there means the
		// build (not the file-copy install step) is what actually failed.
		reportedPhase = PhaseBuild
	}
	base.Status = evidence.StatusFailed
	base.Phase = reportedPhase
	base.Diagnostics = chosenDiags
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
