// deadopt_events.go — redaction-safe supervisor-events.log emit helpers for
// the atomic all-clients de-adopt lifecycle.
//
// Bodies contain names, counts, fixed phase labels, the expected manifest hash,
// and the mandatory conflict-discard warning only. They never accept or emit a
// secret value, snapshot byte, client config body, or raw error.

package api

import (
	"encoding/hex"
	"path/filepath"
	"time"
)

const (
	deAdoptEventSource            = "deadopt"
	deAdoptEventExecuted          = "deadopt-executed"
	deAdoptEventCloseReadyBlocked = "deadopt-close-ready-blocked"
	deAdoptEventClientAccepted    = "deadopt-client-accepted"
	deAdoptEventCloseFailed       = "deadopt-close-failed"

	deAdoptAcceptConflictWarning = "the accepted client's pinned snapshot is DESTROYED at close and its pre-adopt original config + secret-literal spellings are discarded without ever being restored"
)

func emitDeAdoptEvent(severity, event string, body map[string]any) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return
	}
	logger, err := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if err != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.TryEmit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      severity,
		Source:        deAdoptEventSource,
		Event:         event,
		Body:          body,
	})
}

func emitDeAdoptClientAccepted(manifestName, clientName string) {
	emitDeAdoptEvent(SupervisorEventSeverityWarn, deAdoptEventClientAccepted, map[string]any{
		"manifest": manifestName,
		"client":   clientName,
		"warning":  deAdoptAcceptConflictWarning,
	})
}

func emitDeAdoptCloseReadyBlocked(manifestName, manifestHash string, report *DeAdoptReport) {
	failedClients := deAdoptFailedClientNames(report)
	emitDeAdoptEvent(SupervisorEventSeverityWarn, deAdoptEventCloseReadyBlocked, map[string]any{
		"manifest":       manifestName,
		"manifest_hash":  deAdoptEventManifestHash(manifestHash),
		"failed_clients": failedClients,
		"failed_count":   len(failedClients),
		"restored_count": len(report.Restored),
		"accepted_count": len(report.Accepted),
	})
}

func emitDeAdoptCloseFailed(manifestName, manifestHash, phase string, report *DeAdoptReport) {
	emitDeAdoptEvent(SupervisorEventSeverityError, deAdoptEventCloseFailed, map[string]any{
		"manifest":       manifestName,
		"manifest_hash":  deAdoptEventManifestHash(manifestHash),
		"phase":          phase,
		"restored_count": len(report.Restored),
		"accepted_count": len(report.Accepted),
	})
}

func emitDeAdoptExecuted(manifestName, manifestHash string, report *DeAdoptReport, sharedSecretKeys []string) {
	emitDeAdoptEvent(SupervisorEventSeverityInfo, deAdoptEventExecuted, map[string]any{
		"manifest":                   manifestName,
		"manifest_hash":              deAdoptEventManifestHash(manifestHash),
		"restored_clients":           append([]string(nil), report.Restored...),
		"restored_count":             len(report.Restored),
		"accepted_clients":           append([]string(nil), report.Accepted...),
		"accepted_count":             len(report.Accepted),
		"shared_secret_keys_skipped": append([]string(nil), sharedSecretKeys...),
		"shared_secret_skip_count":   len(sharedSecretKeys),
	})
}

func deAdoptEventManifestHash(value string) string {
	if len(value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

func deAdoptFailedClientNames(report *DeAdoptReport) []string {
	if report == nil {
		return nil
	}
	names := make([]string, 0, len(report.Failed))
	for _, failure := range report.Failed {
		names = append(names, failure.Client)
	}
	return names
}
