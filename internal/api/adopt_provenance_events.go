// adopt_provenance_events.go — supervisor-events.log emit helpers for the
// adopt-side durable provenance lifecycle (design "Observability").
//
// All bodies are REDACTED by construction: manifest names, client names,
// present/absent counts, snapshot PATHS, and a manifest hash only — NEVER secret
// VALUES or config contents. This mirrors emitAdoptExecutedEvent, which logs
// secret_routed_keys NAMES and never values (adopt.go:537-551). The helpers
// accept only names/counts/paths, so a caller cannot pass a secret value through
// them.
//
// Emit precedent: emitAdoptExecutedEvent (adopt.go:527-553) — resolve state dir,
// OpenSupervisorEventLog, SupervisorEvent envelope, Source:"adopt". Every emit is
// best-effort (a state-dir / log-open / emit failure is swallowed exactly as
// emitAdoptExecutedEvent swallows it) — an audit-row miss must never fail an
// adopt.

package api

import (
	"path/filepath"
	"time"
)

// adoptProvenanceEventSource is the supervisor-events.log `source` shared by
// every provenance event — the same `adopt` source emitAdoptExecutedEvent uses.
const adoptProvenanceEventSource = "adopt"

// adoptOrphanReapTriggerUpsert marks an orphan-reaped event fired by the capture
// UPSERT (a same-manifest re-run replacing a pre-crash orphan);
// adoptOrphanReapTriggerGC marks one fired by the bounded cross-manifest GC
// (gcOrphanedAdoptingProvenance).
const (
	adoptOrphanReapTriggerUpsert = "upsert"
	adoptOrphanReapTriggerGC     = "gc"
)

// adoptReapFailPhaseRow and adoptReapFailPhaseRowlessDir name the two GC reap
// stages that emitAdoptProvenanceReapFailed distinguishes (P3-3): the Phase-2
// row-reap and the Phase-3 rowless-dir snapshot removal. Single-owned here so the
// wire points and the tests share one string.
const (
	adoptReapFailPhaseRow        = "gc-row"
	adoptReapFailPhaseRowlessDir = "gc-rowless-dir"
	// adoptReapFailPhaseLeasePathError names a GC skip where the per-manifest lease
	// path could NOT be resolved / acquired due to an ERROR (F1) — notably a legacy
	// ".lease"-suffixed manifest that a pre-P3-1 build wrote to disk, whose lease path
	// now fails the reserved-suffix guard. Such an orphan is permanently unreachable by
	// the reaper, so the GC REPORTS it (instead of silently skipping) so an operator can
	// remove adopt-provenance/<name> manually. Distinct from a lease HELD by a live
	// adopt, which stays a legitimate silent skip.
	adoptReapFailPhaseLeasePathError = "gc-lease-path-error"
)

// emitAdoptProvenanceEvent is the single owner of the provenance event envelope
// (severity + event + already-redacted body) on the `adopt` source.
func emitAdoptProvenanceEvent(severity, event string, body map[string]any) {
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		return
	}
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      severity,
		Source:        adoptProvenanceEventSource,
		Event:         event,
		Body:          body,
	})
}

// emitAdoptProvenanceCaptured records a pending `adopting` row + N snapshots.
// Body: manifest, clients (names), present_count, absent_count, snapshot_refs
// (state-dir-relative paths).
func emitAdoptProvenanceCaptured(rec *AdoptProvenanceRecord) {
	if rec == nil {
		return
	}
	var (
		presentCount int
		absentCount  int
		clientNames  = make([]string, 0, len(rec.Clients))
		snapshotRefs = make([]string, 0, len(rec.Clients))
	)
	for _, c := range rec.Clients {
		clientNames = append(clientNames, c.Client)
		switch c.OriginalState {
		case AdoptOriginalStatePresent:
			presentCount++
			if c.SnapshotRef != "" {
				snapshotRefs = append(snapshotRefs, c.SnapshotRef)
			}
		case AdoptOriginalStateAbsent:
			absentCount++
		}
	}
	emitAdoptProvenanceEvent(SupervisorEventSeverityInfo, "adopt-provenance-captured", map[string]any{
		"manifest":      rec.ManifestName,
		"clients":       clientNames,
		"present_count": presentCount,
		"absent_count":  absentCount,
		"snapshot_refs": snapshotRefs,
	})
}

// emitAdoptProvenanceCommitted records adopting -> adopted. Body: manifest,
// manifest_hash.
func emitAdoptProvenanceCommitted(manifestName, manifestHash string) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityInfo, "adopt-provenance-committed", map[string]any{
		"manifest":      manifestName,
		"manifest_hash": manifestHash,
	})
}

// emitAdoptProvenanceCaptureFailed records a fail-closed capture failure (before
// any adopt mutation). Body: manifest, client, reason (a PATH-FREE class the
// caller supplies). Called by the Phase C seam.
func emitAdoptProvenanceCaptureFailed(manifestName, client, reason string) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityError, "adopt-provenance-capture-failed", map[string]any{
		"manifest": manifestName,
		"client":   client,
		"reason":   reason,
	})
}

// emitAdoptProvenanceAbort records a row + snapshots removed during adopt failure
// cleanup. Body: manifest, reason.
func emitAdoptProvenanceAbort(manifestName, reason string) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityWarn, "adopt-provenance-abort", map[string]any{
		"manifest": manifestName,
		"reason":   reason,
	})
}

// emitAdoptProvenancePreserved records that an adopt Install FAILED with an
// INCOMPLETE client-config rollback (≥1 client whose pre-adopt restoration could
// not be confirmed), so the whole partially-committed state (row `adopting` +
// snapshots + manifest + routed vault keys) was PRESERVED rather than aborted —
// keeping it recoverable (bug 2026-07-12). Body: manifest, clients (NAMES of the
// clients whose pre-adopt restoration could not be confirmed), client_count.
// NAMES/COUNTS only — never secret values or config contents.
func emitAdoptProvenancePreserved(manifestName string, clientNames []string) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityWarn, "adopt-provenance-preserved", map[string]any{
		"manifest":     manifestName,
		"clients":      append([]string(nil), clientNames...),
		"client_count": len(clientNames),
	})
}

// emitAdoptProvenanceCommitFailed records that Install committed but the flip
// write failed; the row is left `adopting` (recoverable). Body: manifest. Called
// by the Phase C seam.
func emitAdoptProvenanceCommitFailed(manifestName string) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityWarn, "adopt-provenance-commit-failed", map[string]any{
		"manifest": manifestName,
	})
}

// emitAdoptProvenanceOrphanReaped records that the capture UPSERT (or the Phase D
// GC) removed a stale `adopting` row + snapshot dir. Body: manifest, age_seconds,
// trigger ("upsert" | "gc").
func emitAdoptProvenanceOrphanReaped(manifestName string, ageSeconds float64, trigger string) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityWarn, "adopt-provenance-orphan-reaped", map[string]any{
		"manifest":    manifestName,
		"age_seconds": ageSeconds,
		"trigger":     trigger,
	})
}

// emitAdoptProvenanceReapFailed records that the GC could NOT complete a reap —
// either the Phase-2 row-reap (reapAdoptProvenanceRow) or the Phase-3 rowless-dir
// snapshot removal (removeAdoptSnapshots) returned an error — so a stale,
// secret-bearing `adopting` orphan (or rowless snapshot dir) remains on disk with no
// operator signal until the next GC pass (previously a SILENT `else` branch, P3-3).
// Body: manifest, phase ("gc-row" | "gc-rowless-dir"), reason (the returned error's
// path/class string). NAMES/PATHS/COUNTS only — reapAdoptProvenanceRow /
// removeAdoptSnapshots errors are path/class only and NEVER carry a secret value or
// config content, matching the module's redaction contract.
func emitAdoptProvenanceReapFailed(manifestName, phase, reason string) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityWarn, "adopt-provenance-reap-failed", map[string]any{
		"manifest": manifestName,
		"phase":    phase,
		"reason":   reason,
	})
}

// emitAdoptProvenanceReapSkippedManifestPresent records that the GC's mutation-point
// guard (bug 2026-07-11 P1-2 Part 3) REFUSED to reap a CRASH_REAP-classified row
// because a manifest for it exists on disk (a classifier regression, or a manifest
// re-created inside the classify->reap window) — so a committed adopt's secret
// snapshots were preserved rather than destroyed. Body: manifest, age_seconds
// (NAMES/COUNTS only — never secret values or config contents).
func emitAdoptProvenanceReapSkippedManifestPresent(manifestName string, ageSeconds float64) {
	emitAdoptProvenanceEvent(SupervisorEventSeverityWarn, "adopt-provenance-reap-skipped-manifest-present", map[string]any{
		"manifest":    manifestName,
		"age_seconds": ageSeconds,
	})
}
