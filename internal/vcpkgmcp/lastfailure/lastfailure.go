package lastfailure

import (
	"context"
	"errors"
	"io/fs"
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
//
// It is deliberately a two-line composition: lastFailure computes through
// producer-bounded collectors and boundResponse asserts the final encoding, so every
// one of lastFailure's fifteen returns is bounded by construction rather than
// by remembering to. The predecessor bounded the answer inline, just before
// the single `return base` at the end of the happy path, so the fourteen early
// returns — one of which carries an unbounded FailedTarget joined from a
// wrapper's failed_ports list — were never bounded at all.
//
// The split also makes the budget's central safety property structural: a
// function that receives a finished Result cannot change the verdict it was
// handed. See boundResponse.
func LastFailure(args Args, deps Deps) Result {
	return LastFailureContext(context.Background(), args, deps)
}

// LastFailureContext is the cancellation-aware production entry used by the
// shared MCP server. The legacy LastFailure entry remains deterministic for
// package callers and tests.
func LastFailureContext(ctx context.Context, args Args, deps Deps) Result {
	return lastFailureWithLimits(ctx, args, deps, defaultResponseLimits)
}

func lastFailureWithLimits(ctx context.Context, args Args, deps Deps, limits responseLimits) Result {
	state := newCallState(limits)
	if err := validateArgs(args, limits); err != nil {
		state.report.Completeness.Arguments = false
		res := unknownResult(ReasonArgsInvalid, state.ev, []Note{NoteProducerLimitEngaged}, nil)
		return finalizeProjectedResult(res, state)
	}
	res := lastFailure(ctx, args, deps, state)
	return finalizeProjectedResult(res, state)
}

// lastFailure computes the answer while enforcing producer work/retention
// limits. A limit never manufactures a confident verdict: incomplete evidence
// selects an explicit unknown reason before the final wire backstop runs.
func lastFailure(ctx context.Context, args Args, deps Deps, state *callState) Result {
	ev := state.ev
	var notes []Note
	var sources []ContextSource
	if ctx.Err() != nil {
		return unknownResult(ReasonResourceCancelled, ev, notes, sources)
	}

	// --- Step 1: optional wrapper enrichment -------------------------------
	// Four DISTINCT observations, kept apart because each names a different
	// operator remedy and none of them may borrow another's claim: nothing
	// was supplied (the tool never looked), a supplied path is absent, a
	// supplied path could not be read, or its content yielded nothing.
	var wrapperInfo WrapperInfo
	wrapperOK := false
	wrapperPath := strings.TrimSpace(args.BuildFailedLog)
	switch {
	case wrapperPath == "":
		notes = append(notes, NoteWrapperNotSupplied)
	default:
		data, truncated, err := readMetadataLimitedContext(ctx, deps.FS, wrapperPath, state.limits.metadataBytes)
		switch {
		case errors.Is(err, context.Canceled):
			return unknownResult(ReasonResourceCancelled, state.ev, notes, sources)
		case err == nil && truncated:
			state.report.Completeness.Metadata = false
			state.report.Omitted.WrapperBytesAtLeast++
			return unknownResult(ReasonMetadataLimitExceeded, state.ev,
				append(notes, NoteProducerLimitEngaged), sources)
		case err == nil:
			var parseErr error
			wrapperInfo, wrapperOK, parseErr = parseWrapperContentWithLimits(data, state.limits)
			state.report.Omitted.FailedPortEntries += wrapperInfo.FailedPortsDropped
			state.report.Omitted.OverlayEntries += wrapperInfo.OverlayPortsDropped
			if wrapperInfo.FailedPortsDropped > 0 || wrapperInfo.OverlayPortsDropped > 0 || wrapperInfo.CommandTruncated {
				state.report.Completeness.Metadata = false
			}
			switch {
			case wrapperOK:
				notes = append(notes, NoteWrapperUsedForContext)
				sources = append(sources, SourceWrapperSummary)
				state.addPath(wrapperPath)
			case parseErr != nil:
				// The scan ABORTED — bufio.Scanner gave up (a line beyond the
				// 4 MiB buffer, or a read error). We did NOT see the file end to
				// end, so we cannot claim anything about its shape.
				//
				// NoteWrapperMalformed's own contract three lines up in types.go
				// is "the wrapper file WAS read end to end and nothing
				// recognizable was recovered — a verified fact about content".
				// Discarding parseErr here asserted exactly that fact about a
				// file we had abandoned mid-read, which is the same mistake
				// NoteWrapperUnreadable's doc comment was written to prevent:
				// it sends the operator to fix a wrapper script when nothing is
				// wrong with the script.
				notes = append(notes, NoteWrapperUnreadable)
			default:
				notes = append(notes, NoteWrapperMalformed)
			}
		case errors.Is(err, fs.ErrNotExist):
			notes = append(notes, NoteWrapperNotFound)
		default:
			notes = append(notes, NoteWrapperUnreadable)
		}
	}

	// --- Step 2: determine port ---------------------------------------------
	port := strings.TrimSpace(args.Port)
	triplet := strings.TrimSpace(args.Triplet)
	if port == "" {
		switch {
		case wrapperOK && wrapperInfo.FailedPortsDropped > 0:
			res := unknownResult(ReasonMetadataLimitExceeded, state.ev,
				append(notes, NoteProducerLimitEngaged), sources)
			return res
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
			res.FailedTarget = boundedJoin(wrapperInfo.FailedPorts, ", ", state.limits.pathBytes)
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
		notes = append(notes, NoteWrapperFailedPortsCompletenessUnproven)
	}

	// --- Step 3: determine buildtrees root -----------------------------------
	buildtreesRoot := strings.TrimSpace(args.BuildtreesRoot)
	if buildtreesRoot == "" && wrapperOK && wrapperInfo.BuildtreesRoot != "" {
		// Also covered by the absolute-root gate below: vcpkg resolves a
		// relative --x-buildtrees-root against the shell that ran it, whose
		// working directory this tool has no way to know.
		buildtreesRoot = wrapperInfo.BuildtreesRoot
	}
	// vcpkgRoot is the vcpkg root this call actually knows, whether it was
	// supplied or discovered. It is tracked separately from buildtreesRoot
	// because the two are independent: buildtrees can be located directly
	// (buildtrees_root, or a wrapper's --x-buildtrees-root) with no vcpkg root
	// ever resolved. The overlay-chain fallback needs the vcpkg root, and
	// previously read args.Root only — so the documented
	// vcpkg-configuration.json source was silently skipped on the discovered-
	// root path, and consulted on a relative args.Root that binds to the hub
	// daemon's working directory. Both are fixed by resolving it once here.
	vcpkgRoot := ""
	if r := strings.TrimSpace(args.Root); r != "" && absoluteRoot(r) {
		vcpkgRoot = r
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
			state.addEvidencePaths(discRes.Evidence.Paths)
			res := unknownResult(ReasonVcpkgRootNotResolved, ev, notes, sources)
			return res
		}
		vcpkgRoot = discRes.Root
		buildtreesRoot = filepath.Join(discRes.Root, "buildtrees")
	}
	if !absoluteRoot(buildtreesRoot) {
		res := unknownResult(ReasonRelativeRoot, ev, notes, sources)
		res.FailedTarget = port
		return res
	}
	sources = append(sources, SourceBuildtrees)
	state.addPath(buildtreesRoot)

	// --- Step 4: buildtrees root must exist (else --clean-buildtrees-after-build
	// almost certainly removed it) -------------------------------------------
	switch p, _ := probeDir(deps.FS, buildtreesRoot); p {
	case evidence.PresenceAbsent:
		res := unknownResult(ReasonBuildtreesRootAbsent, ev, notes, sources)
		res.FailedTarget = port
		if wrapperOK && wrapperInfo.ExitCode != nil {
			res.ExitCode = wrapperInfo.ExitCode
		}
		chain, notesOut := resolveOverlayChain(ctx, args, wrapperInfo, wrapperOK, vcpkgRoot, deps, state)
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
	state.addPath(portDir)

	// --- Step 6: resolve triplet if still unknown ----------------------------
	if triplet == "" {
		t, candidates, ok, limitExceeded, derr := detectTripletFromPortDir(deps.FS, portDir, state.limits.directoryEntries)
		switch {
		case derr != nil:
			res := unknownResult(ReasonPortDirUnreadable, state.ev, notes, sources)
			res.FailedTarget = port
			return res
		case limitExceeded:
			state.report.HighWater.DirectoryEntries = state.limits.directoryEntries
			state.report.Completeness.DirectoryEntries = false
			state.report.Omitted.DirectoryEntriesAtLeast++
			res := unknownResult(ReasonArtifactLimitExceeded, state.ev,
				append(notes, NoteProducerLimitEngaged), sources)
			res.FailedTarget = port
			return res
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
	classification, err := classifyPortDir(deps.FS, portDir, triplet, state.limits)
	if err != nil {
		res := unknownResult(ReasonPortDirUnreadable, ev, notes, sources)
		res.FailedTarget = port
		return res
	}
	state.report.HighWater.DirectoryEntries = classification.entriesExamined
	state.report.HighWater.RelevantLogs = classification.relevantLogsRetained
	if classification.directoryLimitExceeded {
		state.report.Completeness.DirectoryEntries = false
		state.report.Omitted.DirectoryEntriesAtLeast++
		res := unknownResult(ReasonArtifactLimitExceeded, state.ev,
			append(notes, NoteProducerLimitEngaged), sources)
		res.FailedTarget = port
		return res
	}
	if classification.relevantLogLimitExceeded {
		state.report.Completeness.RelevantLogs = false
		state.report.Omitted.RelevantLogsAtLeast += classification.relevantLogsDroppedAtLeast
		res := unknownResult(ReasonArtifactLimitExceeded, state.ev,
			append(notes, NoteProducerLimitEngaged), sources)
		res.FailedTarget = port
		return res
	}
	for _, p := range classification.otherLogPaths {
		state.addPath(p)
	}
	if classification.otherLogPathsDropped > 0 {
		state.report.Completeness.LogPaths = false
		state.report.Omitted.LogPaths += classification.otherLogPathsDropped
	}
	if len(classification.phases) == 0 {
		res := unknownResult(ReasonNoPhaseLogsFound, ev, notes, sources)
		res.FailedTarget = port
		paths := newBoundedStringCollector(state.limits.listEntries, state.limits.pathBytes)
		paths.addAll(classification.otherLogPaths)
		res.LogPaths = paths.values
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
	var errSummary, anySummary *phaseSummary
	logPaths := newBoundedStringCollector(state.limits.listEntries, state.limits.pathBytes)
	var unreadableLog, truncatedLog, artifactLimit, cancelled bool
	var totalLogBytes int64
	interrupted := false
	logScanner := newPhaseLogStreamScanner()

	// scan streams one admitted log through a call-scoped reusable scanner and
	// feeds only capped candidates into the phase accumulator. No whole-log
	// byte slice escapes the scanner.
	scan := func(pf phaseLogFile, accumulator *diagnosticAccumulator) bool {
		if ctx.Err() != nil {
			cancelled = true
			return false
		}
		remaining := state.limits.totalLogBytes - totalLogBytes
		if remaining <= 0 {
			artifactLimit = true
			state.report.Completeness.LogBytes = false
			state.report.Omitted.LogBytesAtLeast++
			return false
		}
		readLimit := state.limits.logBytes
		if remaining < readLimit {
			readLimit = remaining
		}
		checkpoint := accumulator.checkpoint()
		scanResult, rerr := logScanner.scan(ctx, deps.FS, pf, readLimit,
			state.limits.diagnosticsPerLogCell, state.limits.commandBytes,
			state.limits.logLineBytes, accumulator)
		if rerr != nil {
			accumulator.restore(checkpoint)
			state.report.Completeness.LogBytes = false
			if errors.Is(rerr, context.Canceled) {
				cancelled = true
			} else {
				unreadableLog = true
			}
			return false
		}
		totalLogBytes += scanResult.bytesRead
		if totalLogBytes > state.report.HighWater.LogBytes {
			state.report.HighWater.LogBytes = totalLogBytes
		}
		if scanResult.logBufferBytes > state.report.HighWater.LogBufferBytes {
			state.report.HighWater.LogBufferBytes = scanResult.logBufferBytes
		}
		if scanResult.truncated {
			if readLimit < state.limits.logBytes {
				artifactLimit = true
				state.report.Completeness.LogBytes = false
				state.report.Omitted.LogBytesAtLeast++
			} else {
				truncatedLog = true
				state.report.Completeness.LogBytes = false
			}
		}
		if scanResult.interrupted {
			interrupted = true
		}
		if scanResult.diagnosticsIncomplete {
			// An over-long diagnostic line was drained without retention. The
			// diagnostic evidence is incomplete, so it cannot support a
			// confident verdict even though interrupt scanning continued.
			truncatedLog = true
			state.report.Completeness.Diagnostics = false
		}
		return true
	}

	for _, ph := range phaseOrder {
		accumulator := newDiagnosticAccumulator(state.limits.diagnosticsPerPhaseCell)
		for _, pf := range classification.phases {
			if pf.Phase != ph {
				continue
			}
			logPaths.add(pf.Path)
			state.addPath(pf.Path)
			scan(pf, accumulator)
		}
		candidates := accumulator.ranked()
		if len(candidates) == 0 {
			continue
		}
		summary := &phaseSummary{phase: ph, candidates: candidates,
			dropped: accumulator.dropped, textCut: accumulator.textCut,
			valueCut: accumulator.valueCut, highWater: accumulator.highWater,
			commands: accumulator.commands}
		if accumulator.highWater > state.report.HighWater.DiagnosticCandidates {
			state.report.HighWater.DiagnosticCandidates = accumulator.highWater
		}
		anySummary = summary
		if summary.hasFailure() {
			errSummary = summary
		}
	}

	// Bonus/fallback: the top-level per-port narration log, only consulted
	// when no primary phase log established a failure.
	if classification.stdoutNarration != "" {
		logPaths.add(classification.stdoutNarration)
		state.addPath(classification.stdoutNarration)
	}
	if errSummary == nil && classification.stdoutNarration != "" {
		accumulator := newDiagnosticAccumulator(state.limits.diagnosticsPerPhaseCell)
		pf := phaseLogFile{Phase: PhaseInstall, Stream: "out", Path: classification.stdoutNarration}
		scan(pf, accumulator)
		if candidates := accumulator.ranked(); len(candidates) > 0 {
			if accumulator.highWater > state.report.HighWater.DiagnosticCandidates {
				state.report.HighWater.DiagnosticCandidates = accumulator.highWater
			}
			summary := &phaseSummary{phase: PhaseInstall, candidates: candidates,
				dropped: accumulator.dropped, textCut: accumulator.textCut,
				valueCut: accumulator.valueCut, highWater: accumulator.highWater,
				commands: accumulator.commands}
			if anySummary == nil {
				anySummary = summary
			}
			if summary.hasFailure() {
				errSummary = summary
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
	if errSummary == nil {
		for _, p := range classification.configureLogYAMLPaths {
			logPaths.add(p)
			state.addPath(p)
			accumulator := newDiagnosticAccumulator(state.limits.diagnosticsPerPhaseCell)
			pf := phaseLogFile{Phase: PhaseConfig, Stream: "out", Path: p}
			if !scan(pf, accumulator) {
				continue
			}
			summary := &phaseSummary{phase: PhaseConfig, candidates: accumulator.ranked(),
				dropped: accumulator.dropped, textCut: accumulator.textCut,
				valueCut: accumulator.valueCut, highWater: accumulator.highWater,
				commands: accumulator.commands}
			if accumulator.highWater > state.report.HighWater.DiagnosticCandidates {
				state.report.HighWater.DiagnosticCandidates = accumulator.highWater
			}
			if !summary.hasFailure() {
				continue
			}
			errSummary = summary
			if anySummary == nil {
				anySummary = summary
			}
			usedCapabilityProbeLog = true
			break
		}
	}
	if ctx.Err() != nil {
		cancelled = true
	}

	// Resolve which set the answer is built from: an error-bearing selection
	// when one exists, otherwise the warning-only evidence.
	chosen := errSummary
	if chosen == nil {
		chosen = anySummary
	}
	var chosenPhase Phase
	var chosenCandidates []diagnosticCandidate
	if chosen != nil {
		chosenPhase, chosenCandidates = chosen.phase, chosen.candidates
	}
	chosenDiags := make([]Diagnostic, len(chosenCandidates))
	for i := range chosenCandidates {
		chosenDiags[i] = chosenCandidates[i].diagnostic
	}

	overlayChain, overlayNotes := resolveOverlayChain(ctx, args, wrapperInfo, wrapperOK, vcpkgRoot, deps, state)
	notes = append(notes, overlayNotes...)
	if usedCapabilityProbeLog {
		notes = append(notes, NoteDiagnosticFromCapabilityProbeLog)
	}

	// exact_command is the reproducible TOP-LEVEL vcpkg invocation and comes
	// only from an authoritative record of it. See Result.ExactCommand: a
	// phase log holds a nested build tool's output, never vcpkg's own command
	// line, so nothing is lifted out of one here.
	exactCommand := ""
	if wrapperOK && wrapperInfo.Command != "" {
		exactCommand = boundedValue(wrapperInfo.Command, state.limits.commandBytes)
		state.addCommand(exactCommand)
	} else {
		notes = append(notes, NoteExactCommandNotRecovered)
	}

	// The build-layer sub-invocation is reported separately, and only from
	// the SAME (phase, configuration) build step that produced the headline
	// diagnostic — so its provenance can be stated, not guessed.
	var buildCommand, diagnosticLog string
	if chosen != nil {
		headline, ok := chosen.headline()
		if ok {
			diagnosticLog = headline.file.Path
			buildCommand = chosen.commandFor(headline.file.Config)
			if buildCommand != "" {
				state.addCommand(buildCommand)
			}
		}
	}
	logPaths.addAll(classification.otherLogPaths)
	finalLogPaths := causalLogPaths(boundedValue(diagnosticLog, state.limits.pathBytes), logPaths.values, state.limits.listEntries)
	if logPaths.dropped > 0 {
		state.report.Completeness.LogPaths = false
		state.report.Omitted.LogPaths += logPaths.dropped
	}
	boundedDiagnostics, responseDropped := applyResponseBudget(chosenDiags)
	diagnosticsDropped := responseDropped
	if chosen != nil {
		diagnosticsDropped += chosen.dropped
		if chosen.textCut {
			notes = append(notes, NoteDiagnosticTextTruncated)
		}
		if chosen.valueCut {
			notes = append(notes, NoteResponseValueTruncated)
		}
	}
	if diagnosticsDropped > 0 {
		notes = append(notes, NoteDiagnosticsTruncatedToBudget)
	}

	base := Result{
		FailedTarget:       port,
		ExactCommand:       exactCommand,
		BuildCommand:       buildCommand,
		DiagnosticLog:      diagnosticLog,
		LogPaths:           finalLogPaths,
		OverlayChain:       overlayChain,
		ContextSource:      dedupSources(sources),
		Notes:              notes,
		Evidence:           ev,
		Diagnostics:        boundedDiagnostics,
		DiagnosticsDropped: diagnosticsDropped,
	}
	if wrapperOK && wrapperInfo.ExitCode != nil {
		base.ExitCode = wrapperInfo.ExitCode
	}

	// Diagnostics found are always returned, whatever the verdict — they are
	// the evidence the caller judges, and withholding them on an `unknown`
	// would force a re-read of the same logs. They are RANKED (errors first,
	// and cause-naming errors ahead of summarising ones) so the actionable
	// line never has to be dug out of a warning flood or out from behind a
	// driver's exit-code report, and the headline error is additionally
	// surfaced on its own.
	base.FirstError = headlineErrorDiagnostic(boundedDiagnostics)

	// No bounding happens here. It is applied to the FINISHED result by
	// boundResponse, which LastFailure routes every return through — see
	// LastFailure's doc for why the bound was moved out of this function.
	//
	// Verdict eligibility comes from an end-to-end scan, while chosenDiags is the
	// bounded ranked set retained from that scan. Per-class cells ensure an error
	// cannot be squeezed out by warnings; any incomplete scan fails closed above.

	reportedPhase := chosenPhase
	if reportedPhase == PhaseInstall {
		// vcpkg's own ninja invocation compiles AND installs in one "install"
		// step; a compiler/linker-shaped diagnostic found there means the
		// build (not the file-copy install step) is what actually failed.
		reportedPhase = PhaseBuild
	}

	// Verdict precedence, strongest positive evidence first.
	switch {
	case cancelled:
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonResourceCancelled
		base.Phase = reportedPhase
		state.report.Completeness.Diagnostics = false

	case interrupted:
		// A ninja user-interrupt overrides any "FAILED:"-shaped match found
		// alongside it — the build was stopped, not broken; reporting it as
		// a defect would misdirect the operator (scout-pass trap 2). This
		// outranks the unreadable-log reason below because it is a positive
		// finding from evidence that WAS read.
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonBuildInterrupted

	case artifactLimit:
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonArtifactLimitExceeded
		base.Phase = reportedPhase
		base.Notes = append(base.Notes, NoteProducerLimitEngaged)
		state.report.Completeness.Diagnostics = false

	case unreadableLog:
		// F9: at least one relevant phase log could not be read. Any of the
		// verdicts below could be changed by its contents, so none of them
		// may be issued. The readable evidence is still returned.
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonPhaseLogUnreadable
		base.Phase = reportedPhase

	case truncatedLog:
		// Same rule for a log only PARTLY examined (over the size bound, or
		// a line the scanner could not take). The unread tail can hold a
		// later error or an interrupt marker, so the confident verdicts below
		// are withheld exactly as they are for an unreadable log.
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonPhaseLogSizeLimitExceeded
		base.Phase = reportedPhase
		state.report.Completeness.Diagnostics = false

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
//
// vcpkgRoot is the root resolved by the caller (supplied or discovered), and
// is "" when this call never resolved one. Each outcome returns a note naming
// the source it actually used, so the caller can verify the chain against
// that source; when NO chain was found the notes say whether every documented
// source was consulted or one was out of reach.
func resolveOverlayChain(ctx context.Context, args Args, wrapperInfo WrapperInfo, wrapperOK bool, vcpkgRoot string, deps Deps, state *callState) ([]string, []Note) {
	if wrapperOK && len(wrapperInfo.OverlayPorts) > 0 {
		if wrapperInfo.OverlayPortsDropped > 0 {
			state.report.Completeness.OverlayChain = false
		}
		return wrapperInfo.OverlayPorts, []Note{NoteOverlayChainFromWrapper}
	}
	if len(args.Overlays) > 0 {
		collector := newBoundedStringCollector(state.limits.overlayEntries, state.limits.pathBytes)
		collector.addAll(args.Overlays)
		if collector.dropped > 0 {
			state.report.Completeness.OverlayChain = false
			state.report.Omitted.OverlayEntries += collector.dropped
		}
		return collector.values, []Note{NoteOverlayChainFromParam}
	}
	if deps.Getenv != nil {
		if val := deps.Getenv("VCPKG_OVERLAY_PORTS"); strings.TrimSpace(val) != "" {
			if len(val) > state.limits.inputScalarBytes {
				state.report.Completeness.OverlayChain = false
				state.report.Omitted.OverlayEntries++
				return nil, []Note{NoteOverlayChainFromEnv, NoteProducerLimitEngaged}
			}
			collector := newBoundedStringCollector(state.limits.overlayEntries, state.limits.pathBytes)
			collector.addAll(filepath.SplitList(val))
			if collector.dropped > 0 {
				state.report.Completeness.OverlayChain = false
				state.report.Omitted.OverlayEntries += collector.dropped
			}
			return collector.values, []Note{NoteOverlayChainFromEnv}
		}
	}
	if vcpkgRoot == "" {
		return nil, []Note{NoteOverlayChainNotSupplied, NoteOverlayChainConfigNotConsulted}
	}
	data, truncated, err := readMetadataLimitedContext(ctx, deps.FS, filepath.Join(vcpkgRoot, "vcpkg-configuration.json"), state.limits.metadataBytes)
	if err == nil && truncated {
		state.report.Completeness.Metadata = false
		state.report.Completeness.OverlayChain = false
		return nil, []Note{NoteOverlayConfigLimitExceeded, NoteProducerLimitEngaged}
	}
	if err == nil {
		chain, dropped, parseErr := parseConfiguredOverlays(data, state.limits)
		if parseErr != nil {
			state.report.Completeness.Metadata = false
			state.report.Completeness.OverlayChain = false
			return nil, []Note{NoteOverlayConfigUnreadable}
		}
		if dropped > 0 {
			state.report.Completeness.OverlayChain = false
			state.report.Omitted.OverlayEntries += dropped
		}
		if len(chain) > 0 {
			return chain, []Note{NoteOverlayChainFromVcpkgConfiguration}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		state.report.Completeness.Metadata = false
		state.report.Completeness.OverlayChain = false
		return nil, []Note{NoteOverlayConfigUnreadable}
	}
	return nil, []Note{NoteOverlayChainNotSupplied}
}

// findRunBuildCommandLine recovers CMake's "Run Build Command(s): ..." line
// from a phase log.
//
// It normalizes through the SAME owner the two diagnostic scanners use
// (normalizeLogLine): this is the third reader of one input class — a captured
// build-tool stream — and a colourized marker would otherwise not be found by
// the substring search, while a colourized TAIL would put escape bytes straight
// into Result.BuildCommand on the wire.
func findRunBuildCommandLine(data []byte) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if command, ok := buildCommandFromNormalizedLine(normalizeLFLogLine(line)); ok {
			return command, true
		}
	}
	return "", false
}

func buildCommandFromNormalizedLine(line string) (string, bool) {
	const marker = "Run Build Command(s): "
	if idx := strings.Index(line, marker); idx >= 0 {
		return strings.TrimSpace(line[idx+len(marker):]), true
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
