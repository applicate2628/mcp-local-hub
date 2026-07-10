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
