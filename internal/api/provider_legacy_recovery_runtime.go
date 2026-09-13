package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// providerLegacyPreInstallRuntimeAbsentFn is the final compatibility proof for
// provider receipts created before provider-install-phase.json existed. New
// receipts never depend on this path. Tests may replace the seam only to model
// a positively empty legacy runtime without starting a real supervisor.
var providerLegacyPreInstallRuntimeAbsentFn = providerLegacyPreInstallRuntimeAbsent

// providerLegacyPreInstallRuntimeAbsent refuses to infer never-started history
// from a missing phase marker and a missing owned descriptor alone. The legacy
// compatibility lane additionally requires a readable supervisor-intent file,
// no live listener on the recorded bridge port, and—when a supervisor is
// reachable—no runtime row for the canonical task. A completely absent intent,
// an owner-probe error, or a status/setup failure stays fail-closed.
func providerLegacyPreInstallRuntimeAbsent(rec *AdoptProvenanceRecord) bool {
	if rec == nil || rec.Port <= 0 {
		return false
	}
	intent, err := loadSupervisorOwnedIntent()
	if err != nil || intent == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if pid, found, ownerErr := LoopbackPortOwnerPIDContext(ctx, rec.Port); ownerErr != nil || found || pid != 0 {
		return false
	}

	rows, statusErr := DialSupervisorIPCStatus(ctx)
	if statusErr != nil {
		// No supervisor generation exists in the old pre-Install crash case. The
		// durable readable intent plus an unbound exact port are the remaining
		// positive compatibility evidence. Local setup/corruption errors are not.
		return errors.Is(statusErr, ErrSupervisorIPCUnavailable)
	}
	expectedTask := canonicalIntentTaskKey("mcp-local-hub-" + rec.ManifestName + "-" + adoptDefaultDaemonName)
	for _, row := range rows {
		if canonicalIntentTaskKey(row.TaskName) == expectedTask ||
			(row.Server == rec.ManifestName && row.Daemon == adoptDefaultDaemonName) {
			return false
		}
	}
	return true
}

// claimProviderInstallNeverStarted is the only production entry to the
// destructive never-started claim. New receipts must have a readable
// not_started/recovery_claimed marker. A physically missing legacy marker may
// reach the old migration inside providerInstallNeverStarted only after the
// additional runtime-absence proof above succeeds.
func claimProviderInstallNeverStarted(rec *AdoptProvenanceRecord) (bool, error) {
	if rec == nil || rec.ProviderSource == nil {
		return false, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	if providerInstallPhaseIsNotStarted(rec.ManifestName) {
		return providerInstallNeverStarted(rec)
	}
	path, err := providerInstallPhasePath(rec.ManifestName)
	if err != nil {
		return false, err
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if providerLegacyPreInstallRuntimeAbsentFn == nil || !providerLegacyPreInstallRuntimeAbsentFn(rec) {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return providerInstallNeverStarted(rec)
}
