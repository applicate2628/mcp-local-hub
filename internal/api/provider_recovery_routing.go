package api

import (
	"errors"
	"io/fs"
	"os"
)

// providerPreManifestRecovery is a read-only routing predicate shared by the
// planner and lease-held executor. An E2 crash changes the operation state but
// not the durable never-started proof, so adopting and de_adopting must recover
// the same lane. New receipts use not_started/recovery_claimed. Markerless
// legacy receipts are admitted only when the narrow runtime-absence proof also
// succeeds; the executor still claims recovery before crossing E2.
func providerPreManifestRecovery(rec *AdoptProvenanceRecord) bool {
	if rec == nil || rec.ProviderSource == nil || rec.ProviderSource.DeAdoptPhase != "" {
		return false
	}
	if rec.OperationState != AdoptOperationStateAdopting && rec.OperationState != AdoptOperationStateDeAdopting {
		return false
	}
	if rec.ProviderSource.DisablePhase != "disable_planned" && rec.ProviderSource.DisablePhase != "disable_applied" {
		return false
	}
	exists, err := adoptManifestExistsFn(rec.ManifestName)
	if err != nil || exists {
		return false
	}
	if providerInstallPhaseIsNotStarted(rec.ManifestName) {
		return true
	}
	path, err := providerInstallPhasePath(rec.ManifestName)
	if err != nil {
		return false
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return providerLegacyPreInstallRuntimeAbsentFn != nil && providerLegacyPreInstallRuntimeAbsentFn(rec)
}
