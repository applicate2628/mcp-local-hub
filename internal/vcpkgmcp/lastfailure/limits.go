package lastfailure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Public input limits are shared with the MCP JSON Schema so validation and
// producer enforcement cannot drift apart.
const (
	MaxInputScalarBytes    = 32 << 10
	MaxPortNameBytes       = 256
	MaxInputOverlayEntries = 64
	maxPhaseLogLineBytes   = 4 << 20
)

// responseLimits is the single owner of every vcpkg_last_failure producer
// budget. Tests inject a smaller value through lastFailureWithLimits; production
// uses defaultResponseLimits and no call site re-types one of these values.
type responseLimits struct {
	inputScalarBytes        int
	portNameBytes           int
	overlayEntries          int
	metadataBytes           int64
	directoryEntries        int
	relevantLogs            int
	logBytes                int64
	totalLogBytes           int64
	logLineBytes            int
	diagnosticsPerLogCell   int
	diagnosticsPerPhaseCell int
	diagnosticsEmitted      int
	diagnosticBytes         int
	listEntries             int
	pathBytes               int
	commandBytes            int
}

func parseConfiguredOverlays(data []byte, limits responseLimits) ([]string, int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, 0, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, 0, fmt.Errorf("configuration root is not an object")
	}
	collector := newBoundedStringCollector(limits.overlayEntries, limits.pathBytes)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, 0, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, 0, fmt.Errorf("configuration key is not a string")
		}
		if key != "overlay-ports" {
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				return nil, 0, err
			}
			continue
		}
		arrayToken, err := decoder.Token()
		if err != nil {
			return nil, 0, err
		}
		if delimiter, ok := arrayToken.(json.Delim); !ok || delimiter != '[' {
			return nil, 0, fmt.Errorf("overlay-ports is not an array")
		}
		for decoder.More() {
			var overlay string
			if err := decoder.Decode(&overlay); err != nil {
				return nil, 0, err
			}
			if len(overlay) > limits.inputScalarBytes {
				collector.dropped++
				continue
			}
			collector.add(overlay)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, 0, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, 0, err
	}
	return collector.values, collector.dropped, nil
}

var defaultResponseLimits = responseLimits{
	inputScalarBytes:        MaxInputScalarBytes,
	portNameBytes:           MaxPortNameBytes,
	overlayEntries:          MaxInputOverlayEntries,
	metadataBytes:           4 << 20,
	directoryEntries:        1024,
	relevantLogs:            64,
	logBytes:                32 << 20,
	totalLogBytes:           256 << 20,
	logLineBytes:            maxPhaseLogLineBytes,
	diagnosticsPerLogCell:   maxDiagnosticsPerLog,
	diagnosticsPerPhaseCell: 200,
	diagnosticsEmitted:      MaxResponseDiagnostics,
	diagnosticBytes:         MaxResponseDiagnosticBytes,
	listEntries:             MaxWireListEntries,
	pathBytes:               MaxWirePathBytes,
	commandBytes:            MaxWireCommandBytes,
}

// Completeness reports whether each evidence class was examined to its natural
// end. A false value never silently becomes ok/failed; the reason identifies
// which producer stopped.
type Completeness struct {
	Arguments        bool `json:"arguments"`
	Metadata         bool `json:"metadata"`
	DirectoryEntries bool `json:"directory_entries"`
	RelevantLogs     bool `json:"relevant_logs"`
	LogBytes         bool `json:"log_bytes"`
	Diagnostics      bool `json:"diagnostics"`
	LogPaths         bool `json:"log_paths"`
	OverlayChain     bool `json:"overlay_chain"`
	Evidence         bool `json:"evidence"`
}

// OmittedCounts reports exact drops where the producer reached end of input and
// lower bounds where a sentinel stopped it. The field names make that
// distinction part of the wire contract.
type OmittedCounts struct {
	DirectoryEntriesAtLeast int `json:"directory_entries_at_least,omitempty"`
	RelevantLogsAtLeast     int `json:"relevant_logs_at_least,omitempty"`
	LogBytesAtLeast         int `json:"log_bytes_at_least,omitempty"`
	WrapperBytesAtLeast     int `json:"wrapper_bytes_at_least,omitempty"`
	OverlayEntries          int `json:"overlay_entries,omitempty"`
	FailedPortEntries       int `json:"failed_port_entries,omitempty"`
	LogPaths                int `json:"log_paths,omitempty"`
	EvidenceItems           int `json:"evidence_items,omitempty"`
}

// ResourceReport is additive response metadata for producer-side bounding.
// DiagnosticsDropped remains the diagnostic-output count owner.
type ResourceReport struct {
	Completeness Completeness  `json:"completeness"`
	Omitted      OmittedCounts `json:"omitted,omitempty"`
	HighWater    HighWater     `json:"high_water"`
}

type HighWater struct {
	DirectoryEntries     int   `json:"directory_entries"`
	RelevantLogs         int   `json:"relevant_logs"`
	LogBytes             int64 `json:"log_bytes"`
	LogBufferBytes       int   `json:"log_buffer_bytes"`
	DiagnosticCandidates int   `json:"diagnostic_candidates"`
}

// completeResourceReport is retained for deterministic below-limit fixtures.
// Production results are always overwritten by finalizeProjectedResult.
func completeResourceReport() ResourceReport {
	return ResourceReport{Completeness: Completeness{
		Arguments: true, Metadata: true, DirectoryEntries: true,
		RelevantLogs: true, LogBytes: true, Diagnostics: true,
		LogPaths: true, OverlayChain: true, Evidence: true,
	}}
}

type callState struct {
	limits       responseLimits
	report       ResourceReport
	ev           evidence.Evidence
	preAdmission bool
}

type boundedStringCollector struct {
	values   []string
	max      int
	maxBytes int
	dropped  int
}

func newBoundedStringCollector(max, maxBytes int) *boundedStringCollector {
	return &boundedStringCollector{max: max, maxBytes: maxBytes}
}

func (c *boundedStringCollector) add(value string) {
	if value == "" {
		return
	}
	value = boundedValue(value, c.maxBytes)
	for _, existing := range c.values {
		if existing == value {
			return
		}
	}
	if len(c.values) >= c.max {
		c.dropped++
		return
	}
	c.values = append(c.values, value)
}

func (c *boundedStringCollector) addAll(values []string) {
	for _, value := range values {
		c.add(value)
	}
}

func newCallState(limits responseLimits) *callState {
	// Completeness is projected once from the terminal stop class. The producer
	// still uses this private optimistic snapshot to record concrete limits; it
	// is never serialized directly.
	return &callState{limits: limits, report: ResourceReport{Completeness: Completeness{
		Arguments: true, Metadata: true, DirectoryEntries: true,
		RelevantLogs: true, LogBytes: true, Diagnostics: true,
		LogPaths: true, OverlayChain: true, Evidence: true,
	}}}
}

// ResourceResult returns the normal tri-state result for admission/cancellation
// outcomes that happen before producer execution.
func ResourceResult(reason Reason) Result {
	state := newCallState(defaultResponseLimits)
	state.preAdmission = true
	result := unknownResult(reason, state.ev, nil, nil)
	return finalizeProjectedResult(result, state)
}

type evidenceDomainState uint8

const (
	domainNotStarted evidenceDomainState = iota
	domainNotApplicable
	domainSettledComplete
	domainLimited
	domainUnreadable
	domainCancelled
	domainInvalid
	domainInProgress
)

type stopClass uint8

const (
	stopArgumentsInvalid stopClass = iota + 1
	stopResourceBusy
	stopResourceCancelled
	stopMetadataLimited
	stopMultipleFailedPorts
	stopPortNotSpecified
	stopInvalidPort
	stopWrapperConfirmsNotFailed
	stopRootRelative
	stopRootUnresolved
	stopBuildtreesAbsent
	stopBuildtreesUnreadable
	stopPortDirectoryAbsent
	stopPortDirectoryUnreadable
	stopTripletAmbiguous
	stopDirectoryLimited
	stopRelevantLogsLimited
	stopVerifiedNoPhaseLogs
	stopScanInterrupted
	stopScanArtifactLimited
	stopScanLogUnreadable
	stopScanLogSizeLimited
	stopScanCapabilityProbeOnly
	stopScanNoDiagnostic
	stopScanNoFailure
	stopScanFailed
	stopCausalityViolation
)

var allStopClasses = []stopClass{
	stopArgumentsInvalid,
	stopResourceBusy,
	stopResourceCancelled,
	stopMetadataLimited,
	stopMultipleFailedPorts,
	stopPortNotSpecified,
	stopInvalidPort,
	stopWrapperConfirmsNotFailed,
	stopRootRelative,
	stopRootUnresolved,
	stopBuildtreesAbsent,
	stopBuildtreesUnreadable,
	stopPortDirectoryAbsent,
	stopPortDirectoryUnreadable,
	stopTripletAmbiguous,
	stopDirectoryLimited,
	stopRelevantLogsLimited,
	stopVerifiedNoPhaseLogs,
	stopScanInterrupted,
	stopScanArtifactLimited,
	stopScanLogUnreadable,
	stopScanLogSizeLimited,
	stopScanCapabilityProbeOnly,
	stopScanNoDiagnostic,
	stopScanNoFailure,
	stopScanFailed,
	stopCausalityViolation,
}

type evidenceProjection struct {
	arguments        evidenceDomainState
	metadata         evidenceDomainState
	directoryEntries evidenceDomainState
	relevantLogs     evidenceDomainState
	logBytes         evidenceDomainState
	diagnostics      evidenceDomainState
	logPaths         evidenceDomainState
	overlayChain     evidenceDomainState
	evidence         evidenceDomainState
	status           evidence.Status
	reason           Reason
}

func projection(
	arguments, metadata, directoryEntries, relevantLogs, logBytes, diagnostics,
	logPaths, overlayChain, evidenceState evidenceDomainState,
	status evidence.Status, reason Reason,
) evidenceProjection {
	return evidenceProjection{
		arguments: arguments, metadata: metadata,
		directoryEntries: directoryEntries, relevantLogs: relevantLogs,
		logBytes: logBytes, diagnostics: diagnostics, logPaths: logPaths,
		overlayChain: overlayChain, evidence: evidenceState,
		status: status, reason: reason,
	}
}

func projectStopClass(class stopClass, state *callState, result Result) (evidenceProjection, bool) {
	const (
		ns = domainNotStarted
		na = domainNotApplicable
		c  = domainSettledComplete
		l  = domainLimited
		u  = domainUnreadable
		x  = domainCancelled
		i  = domainInvalid
	)
	m := projectedMetadataState(state, result)
	p := projectedLogPathState(state, result)
	o := projectedOverlayState(state, result)
	e := projectedEvidenceState(state)

	switch class {
	case stopArgumentsInvalid:
		return projection(i, na, na, na, na, na, na, na, na,
			evidence.StatusUnknown, ReasonArgsInvalid), true
	case stopResourceBusy:
		return projection(na, na, na, na, na, na, na, na, na,
			evidence.StatusUnknown, ReasonResourceBusy), true
	case stopResourceCancelled:
		if state.preAdmission {
			return projection(na, na, na, na, na, na, na, na, na,
				evidence.StatusUnknown, ReasonResourceCancelled), true
		}
		return projection(c, cancelledMetadataState(m), ns, ns, x, x, p, o, e,
			evidence.StatusUnknown, ReasonResourceCancelled), true
	case stopMetadataLimited:
		return projection(c, l, ns, ns, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonMetadataLimitExceeded), true
	case stopMultipleFailedPorts:
		return projection(c, m, na, na, na, na, na, na, e,
			evidence.StatusUnknown, ReasonMultipleFailedPortsAmbiguous), true
	case stopPortNotSpecified:
		return projection(c, m, na, na, na, na, na, na, e,
			evidence.StatusUnknown, ReasonPortNotSpecified), true
	case stopInvalidPort:
		return projection(c, m, na, na, na, na, na, na, e,
			evidence.StatusUnknown, ReasonInvalidPortName), true
	case stopWrapperConfirmsNotFailed:
		return projection(c, c, na, na, na, na, na, na, e,
			evidence.StatusOK, ""), true
	case stopRootRelative:
		return projection(c, m, ns, ns, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonRelativeRoot), true
	case stopRootUnresolved:
		return projection(c, m, ns, ns, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonVcpkgRootNotResolved), true
	case stopBuildtreesAbsent:
		return projection(c, m, na, na, na, na, na, o, e,
			evidence.StatusUnknown, ReasonBuildtreesRootAbsent), true
	case stopBuildtreesUnreadable:
		return projection(c, m, u, ns, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonBuildtreesRootUnreadable), true
	case stopPortDirectoryAbsent:
		return projection(c, m, na, na, na, na, na, na, e,
			evidence.StatusUnknown, ReasonPortDirNotFound), true
	case stopPortDirectoryUnreadable:
		return projection(c, m, u, ns, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonPortDirUnreadable), true
	case stopTripletAmbiguous:
		return projection(c, m, c, ns, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonTripletAmbiguous), true
	case stopDirectoryLimited:
		return projection(c, m, l, ns, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonArtifactLimitExceeded), true
	case stopRelevantLogsLimited:
		return projection(c, m, c, l, ns, ns, ns, ns, e,
			evidence.StatusUnknown, ReasonArtifactLimitExceeded), true
	case stopVerifiedNoPhaseLogs:
		return projection(c, m, c, c, na, na, c, na, e,
			evidence.StatusUnknown, ReasonNoPhaseLogsFound), true
	case stopScanInterrupted:
		// An observed interruption decides the terminal reason, but it cannot
		// manufacture completeness for a producer domain that had already
		// exhausted its byte or diagnostic budget. In particular, a retained
		// prefix can contain the interrupt marker after log_bytes became
		// limited; diagnostics_dropped_exact must remain false in that case.
		return projection(c, m, c, c,
			projectedScanCompleteness(state.report.Completeness.LogBytes),
			projectedScanCompleteness(state.report.Completeness.Diagnostics),
			p, o, e,
			evidence.StatusUnknown, ReasonBuildInterrupted), true
	case stopScanArtifactLimited:
		return projection(c, m, c, c, l, l, p, o, e,
			evidence.StatusUnknown, ReasonArtifactLimitExceeded), true
	case stopScanLogUnreadable:
		return projection(c, m, c, c, u, u, p, o, e,
			evidence.StatusUnknown, ReasonPhaseLogUnreadable), true
	case stopScanLogSizeLimited:
		return projection(c, m, c, c, l, l, p, o, e,
			evidence.StatusUnknown, ReasonPhaseLogSizeLimitExceeded), true
	case stopScanCapabilityProbeOnly:
		return projection(c, m, c, c, c, c, p, o, e,
			evidence.StatusUnknown, ReasonCapabilityProbeOnly), true
	case stopScanNoDiagnostic:
		return projection(c, m, c, c, c, c, p, o, e,
			evidence.StatusUnknown, ReasonNoDiagnosticFound), true
	case stopScanNoFailure:
		return projection(c, m, c, c, c, c, p, o, e,
			evidence.StatusUnknown, ReasonNoFailureDiagnostic), true
	case stopScanFailed:
		return projection(c, m, c, c, c, c, p, o, e,
			evidence.StatusFailed, ""), true
	case stopCausalityViolation:
		return projection(ns, ns, ns, ns, ns, ns, ns, ns, ns,
			evidence.StatusUnknown, ReasonCausalityInvariantViolation), true
	}
	return evidenceProjection{}, false
}

func classifyStopClass(result Result, state *callState) (stopClass, bool) {
	if state.preAdmission {
		switch result.Reason {
		case ReasonResourceBusy:
			return stopResourceBusy, true
		case ReasonResourceCancelled:
			return stopResourceCancelled, true
		default:
			return 0, false
		}
	}
	switch result.Status {
	case evidence.StatusOK:
		if result.Reason == "" && resultHasNote(result, NoteWrapperConfirmsNoFailure) {
			return stopWrapperConfirmsNotFailed, true
		}
		return 0, false
	case evidence.StatusFailed:
		if result.Reason == "" {
			return stopScanFailed, true
		}
		return 0, false
	case evidence.StatusUnknown:
	default:
		return 0, false
	}
	switch result.Reason {
	case ReasonArgsInvalid:
		return stopArgumentsInvalid, true
	case ReasonResourceCancelled:
		return stopResourceCancelled, true
	case ReasonMetadataLimitExceeded:
		return stopMetadataLimited, true
	case ReasonMultipleFailedPortsAmbiguous:
		return stopMultipleFailedPorts, true
	case ReasonPortNotSpecified:
		return stopPortNotSpecified, true
	case ReasonInvalidPortName:
		return stopInvalidPort, true
	case ReasonRelativeRoot:
		return stopRootRelative, true
	case ReasonVcpkgRootNotResolved:
		return stopRootUnresolved, true
	case ReasonBuildtreesRootAbsent:
		return stopBuildtreesAbsent, true
	case ReasonBuildtreesRootUnreadable:
		return stopBuildtreesUnreadable, true
	case ReasonPortDirNotFound:
		return stopPortDirectoryAbsent, true
	case ReasonPortDirUnreadable:
		return stopPortDirectoryUnreadable, true
	case ReasonTripletAmbiguous:
		return stopTripletAmbiguous, true
	case ReasonNoPhaseLogsFound:
		return stopVerifiedNoPhaseLogs, true
	case ReasonBuildInterrupted:
		return stopScanInterrupted, true
	case ReasonPhaseLogUnreadable:
		return stopScanLogUnreadable, true
	case ReasonPhaseLogSizeLimitExceeded:
		return stopScanLogSizeLimited, true
	case ReasonCapabilityProbeOnly:
		return stopScanCapabilityProbeOnly, true
	case ReasonNoDiagnosticFound:
		return stopScanNoDiagnostic, true
	case ReasonNoFailureDiagnostic:
		return stopScanNoFailure, true
	case ReasonCausalityInvariantViolation:
		return stopCausalityViolation, true
	case ReasonArtifactLimitExceeded:
		switch {
		case !state.report.Completeness.DirectoryEntries:
			return stopDirectoryLimited, true
		case !state.report.Completeness.RelevantLogs:
			return stopRelevantLogsLimited, true
		case !state.report.Completeness.LogBytes || !state.report.Completeness.Diagnostics:
			return stopScanArtifactLimited, true
		default:
			return 0, false
		}
	default:
		return 0, false
	}
}

func finalizeProjectedResult(result Result, state *callState) Result {
	result.Evidence = state.ev
	// Causality is itself a raw observation consumed by the same projector.
	// Run it before classification so boundResponse cannot create a second,
	// contradictory public status after resources have been projected.
	result = validateCausality(result)
	class, classified := classifyStopClass(result, state)
	projected, ok := projectStopClass(class, state, result)
	if !classified || !ok {
		projected, _ = projectStopClass(stopCausalityViolation, state, result)
		if !resultHasNote(result, NoteCausalityInvariantViolated) {
			result.Notes = append(result.Notes, NoteCausalityInvariantViolated)
		}
	}
	result.Status = projected.status
	result.Reason = projected.reason
	result.Resources = state.report
	result.Resources.Completeness = projected.completeness()
	result.DiagnosticsDroppedExact = projected.diagnosticsDroppedExact()
	return boundResponse(result)
}

// projectedScanCompleteness preserves the producer-owned completeness fact
// when a terminal outcome is projected. It deliberately maps every incomplete
// producer state to limited: ResourceReport exposes an incomplete boolean,
// while the stop class continues to own the more specific terminal reason.
func projectedScanCompleteness(complete bool) evidenceDomainState {
	if complete {
		return domainSettledComplete
	}
	return domainLimited
}

func (p evidenceProjection) completeness() Completeness {
	return Completeness{
		Arguments:        domainComplete(p.arguments),
		Metadata:         domainComplete(p.metadata),
		DirectoryEntries: domainComplete(p.directoryEntries),
		RelevantLogs:     domainComplete(p.relevantLogs),
		LogBytes:         domainComplete(p.logBytes),
		Diagnostics:      domainComplete(p.diagnostics),
		LogPaths:         domainComplete(p.logPaths),
		OverlayChain:     domainComplete(p.overlayChain),
		Evidence:         domainComplete(p.evidence),
	}
}

func (p evidenceProjection) diagnosticsDroppedExact() bool {
	if p.directoryEntries == domainNotApplicable &&
		p.relevantLogs == domainNotApplicable &&
		p.logBytes == domainNotApplicable &&
		p.diagnostics == domainNotApplicable {
		return true
	}
	return p.directoryEntries == domainSettledComplete &&
		p.relevantLogs == domainSettledComplete &&
		(p.logBytes == domainSettledComplete || p.logBytes == domainNotApplicable) &&
		(p.diagnostics == domainSettledComplete || p.diagnostics == domainNotApplicable)
}

func domainComplete(state evidenceDomainState) bool {
	return state == domainNotApplicable || state == domainSettledComplete
}

func projectedMetadataState(state *callState, result Result) evidenceDomainState {
	if !state.report.Completeness.Metadata {
		return domainLimited
	}
	switch {
	case resultHasNote(result, NoteWrapperUsedForContext):
		return domainSettledComplete
	case resultHasNote(result, NoteWrapperUnreadable),
		resultHasNote(result, NoteWrapperMalformed),
		resultHasNote(result, NoteWrapperNotFound):
		return domainUnreadable
	case resultHasNote(result, NoteWrapperNotSupplied):
		return domainNotApplicable
	default:
		return domainNotStarted
	}
}

func cancelledMetadataState(state evidenceDomainState) evidenceDomainState {
	if state == domainNotStarted || state == domainInProgress {
		return domainCancelled
	}
	return state
}

func projectedLogPathState(state *callState, result Result) evidenceDomainState {
	if !state.report.Completeness.LogPaths {
		return domainLimited
	}
	if len(result.LogPaths) > 0 || result.DiagnosticLog != "" {
		return domainSettledComplete
	}
	return domainNotApplicable
}

func projectedOverlayState(state *callState, result Result) evidenceDomainState {
	if !state.report.Completeness.OverlayChain || resultHasNote(result, NoteOverlayConfigLimitExceeded) {
		return domainLimited
	}
	if resultHasNote(result, NoteOverlayConfigUnreadable) {
		return domainUnreadable
	}
	for _, note := range []Note{
		NoteOverlayChainFromWrapper,
		NoteOverlayChainFromEnv,
		NoteOverlayChainFromParam,
		NoteOverlayChainFromVcpkgConfiguration,
	} {
		if resultHasNote(result, note) {
			return domainSettledComplete
		}
	}
	if resultHasNote(result, NoteOverlayChainNotSupplied) {
		return domainNotApplicable
	}
	return domainNotStarted
}

func projectedEvidenceState(state *callState) evidenceDomainState {
	if !state.report.Completeness.Evidence {
		return domainLimited
	}
	if len(state.ev.Paths) > 0 || len(state.ev.Commands) > 0 {
		return domainSettledComplete
	}
	return domainNotApplicable
}

func resultHasNote(result Result, want Note) bool {
	for _, note := range result.Notes {
		if note == want {
			return true
		}
	}
	return false
}

func (s *callState) addPath(path string) {
	if path == "" {
		return
	}
	for _, existing := range s.ev.Paths {
		if existing == path {
			return
		}
	}
	if len(s.ev.Paths) >= s.limits.listEntries {
		s.report.Completeness.Evidence = false
		s.report.Omitted.EvidenceItems++
		return
	}
	s.ev.Paths = append(s.ev.Paths, boundedValue(path, s.limits.pathBytes))
}

func (s *callState) addCommand(command string) {
	if command == "" {
		return
	}
	if len(s.ev.Commands) >= s.limits.listEntries {
		s.report.Completeness.Evidence = false
		s.report.Omitted.EvidenceItems++
		return
	}
	s.ev.Commands = append(s.ev.Commands, boundedValue(command, s.limits.commandBytes))
}

func (s *callState) addEvidencePaths(paths []string) {
	for _, path := range paths {
		s.addPath(path)
	}
}

func boundedValue(value string, max int) string {
	if len(value) <= max {
		return value
	}
	bounded, _ := truncateWireValue(value, max)
	return bounded
}

func validateArgs(args Args, limits responseLimits) error {
	for name, value := range map[string]string{
		"triplet": args.Triplet, "root": args.Root,
		"buildtrees_root": args.BuildtreesRoot, "build_failed_log": args.BuildFailedLog,
	} {
		if len(value) > limits.inputScalarBytes {
			return fmt.Errorf("%s exceeds %d bytes", name, limits.inputScalarBytes)
		}
	}
	if len(args.Port) > limits.portNameBytes {
		return fmt.Errorf("port exceeds %d bytes", limits.portNameBytes)
	}
	if len(args.Overlays) > limits.overlayEntries {
		return fmt.Errorf("overlays exceeds %d entries", limits.overlayEntries)
	}
	for i, overlay := range args.Overlays {
		if len(overlay) > limits.inputScalarBytes {
			return fmt.Errorf("overlays[%d] exceeds %d bytes", i, limits.inputScalarBytes)
		}
	}
	return nil
}

func boundedJoin(values []string, separator string, max int) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	for i, value := range values {
		if i > 0 {
			if b.Len()+len(separator) > max {
				break
			}
			b.WriteString(separator)
		}
		remaining := max - b.Len()
		if len(value) > remaining {
			prefix, _ := truncateWireValue(value, remaining)
			b.WriteString(prefix)
			break
		}
		b.WriteString(value)
	}
	return b.String()
}
