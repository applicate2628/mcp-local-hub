package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	providerInstallPhaseLeaf            = "provider-install-phase.json"
	providerInstallPhaseVersion         = 1
	providerInstallPhaseNotStarted      = "not_started"
	providerInstallPhaseStarted         = "started"
	providerInstallPhaseRecoveryClaimed = "recovery_claimed"
	providerInstallPhaseManagedSettled  = "managed_settled"
)

type providerInstallPhaseV1 struct {
	Version int    `json:"version"`
	Phase   string `json:"phase"`
}

func providerInstallPhasePath(manifestName string) (string, error) {
	dir, err := adoptSnapshotDir(manifestName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, providerInstallPhaseLeaf), nil
}

func validProviderInstallPhase(phase string) bool {
	switch phase {
	case providerInstallPhaseNotStarted, providerInstallPhaseStarted, providerInstallPhaseRecoveryClaimed, providerInstallPhaseManagedSettled:
		return true
	default:
		return false
	}
}

func encodeProviderInstallPhase(phase string) ([]byte, error) {
	if !validProviderInstallPhase(phase) {
		return nil, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return json.Marshal(providerInstallPhaseV1{Version: providerInstallPhaseVersion, Phase: phase})
}

func readProviderInstallPhase(manifestName string) (string, error) {
	path, err := providerInstallPhasePath(manifestName)
	if err != nil {
		return "", err
	}
	raw, err := ReadStateFileInodeAnchored(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Missing is deliberately UNKNOWN, never equivalent to not_started.
			// Provider provenance written before this marker existed therefore
			// stays fail-closed until an exact managed settlement supplies new,
			// positive evidence through markProviderManagedSettled.
			return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	var record providerInstallPhaseV1
	if err := json.Unmarshal(raw, &record); err != nil || record.Version != providerInstallPhaseVersion {
		return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if !validProviderInstallPhase(record.Phase) {
		return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return record.Phase, nil
}

// providerInstallPhaseIsNotStarted is the read-only proof that Install never
// entered its mutation path. recovery_claimed is included because it is an
// idempotent durable claim of the same historical fact. Missing, legacy,
// corrupt, linked, started, or managed-settled markers are not equivalent to
// never-started history.
func providerInstallPhaseIsNotStarted(manifestName string) bool {
	phase, err := readProviderInstallPhase(manifestName)
	return err == nil && (phase == providerInstallPhaseNotStarted || phase == providerInstallPhaseRecoveryClaimed)
}

func writeProviderInstallPhase(manifestName, phase string) error {
	path, err := providerInstallPhasePath(manifestName)
	if err != nil {
		return err
	}
	raw, err := encodeProviderInstallPhase(phase)
	if err != nil {
		return err
	}
	if err := WriteStateFileBytesAtomic(path, raw); err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: write provider install phase: %w", err)
	}
	observed, err := readProviderInstallPhase(manifestName)
	if err != nil || observed != phase {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return nil
}

// initializeProviderInstallPhase is called before the provider activation CAS.
// The marker lives inside the provenance snapshot directory, so the existing
// snapshots-first abort/close owner removes it with the rest of the recovery
// state. Existing bytes are accepted only when they are the exact initial state;
// an existing corrupt/link/unreadable marker is never rewritten into invented
// history.
func initializeProviderInstallPhase(manifestName string) error {
	path, err := providerInstallPhasePath(manifestName)
	if err != nil {
		return err
	}
	_, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		phase, readErr := readProviderInstallPhase(manifestName)
		if readErr != nil || phase != providerInstallPhaseNotStarted {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		return nil
	case !errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}

	if err := writeProviderInstallPhase(manifestName, providerInstallPhaseNotStarted); err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: initialize provider install phase: %w", err)
	}
	return nil
}

// markProviderInstallStartedForTask is invoked only after the fail-closed
// server-install audit append succeeds and before executeInstallTo may mutate a
// scheduler/client/intent surface. Both the installing manifest identity and its
// canonical task identity must match the provider provenance. Task names alone
// are not injective when server and daemon names may contain hyphens (for
// example foo-bar/default and foo/bar-default produce the same scheduler task).
// The adopted-entries lock serializes the exact not_started -> started
// transition against the recovery claimant below. Once a pre-Install recovery
// has claimed the operation, that manifest's Install must fail before mutation.
func markProviderInstallStartedForTask(manifestName, taskName string) error {
	if manifestName == "" {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	canonical := canonicalIntentTaskKey(taskName)
	return withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		var record *AdoptProvenanceRecord
		for i := range store.Records {
			rec := &store.Records[i]
			if rec.ProviderSource == nil || rec.ManifestName != manifestName {
				continue
			}
			expected := canonicalIntentTaskKey("mcp-local-hub-" + rec.ManifestName + "-" + adoptDefaultDaemonName)
			if expected != canonical {
				continue
			}
			if rec.OperationState == AdoptOperationStateDeAdopting {
				return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
			}
			if rec.OperationState != AdoptOperationStateAdopting {
				continue
			}
			if record != nil {
				return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
			}
			record = rec
		}
		if record == nil {
			return nil
		}
		phase, err := readProviderInstallPhase(record.ManifestName)
		if err != nil {
			return err
		}
		switch phase {
		case providerInstallPhaseStarted:
			return nil
		case providerInstallPhaseNotStarted:
			return writeProviderInstallPhase(record.ManifestName, providerInstallPhaseStarted)
		case providerInstallPhaseRecoveryClaimed, providerInstallPhaseManagedSettled:
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		default:
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
	})
}

// providerInstallNeverStarted is the destructive pre-Install recovery claim.
// The first successful caller atomically converts a durable not_started marker
// to recovery_claimed under adopted-entries.lock. Replays by the same durable
// recovery state remain admitted, while a concurrent/future Install observes
// recovery_claimed and is refused before executeInstallTo mutates anything.
//
// A physically missing marker is UNKNOWN historical state. Present-day absence
// of a manifest, supervisor row, listener, or runtime status can never recreate
// the historical fact that Install was not entered. Markerless receipts therefore
// stay fail-closed here; legacy recovery becomes restore-eligible only after an
// exact terminal managed settlement records managed_settled elsewhere.
func providerInstallNeverStarted(rec *AdoptProvenanceRecord) (bool, error) {
	if rec == nil || rec.ProviderSource == nil {
		return false, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	claimed := false
	err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		var current *AdoptProvenanceRecord
		for i := range store.Records {
			candidate := &store.Records[i]
			if candidate.ManifestName != rec.ManifestName {
				continue
			}
			if current != nil || candidate.ProviderSource == nil || !deAdoptProvenanceIdentityMatches(rec, candidate) {
				return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
			}
			current = candidate
		}
		if current == nil {
			return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
		}
		phase, err := readProviderInstallPhase(rec.ManifestName)
		if err != nil {
			return err
		}
		switch phase {
		case providerInstallPhaseRecoveryClaimed:
			claimed = true
			return nil
		case providerInstallPhaseNotStarted:
			if err := writeProviderInstallPhase(rec.ManifestName, providerInstallPhaseRecoveryClaimed); err != nil {
				return err
			}
			claimed = true
			return nil
		case providerInstallPhaseStarted, providerInstallPhaseManagedSettled:
			return nil
		default:
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// markProviderManagedSettled records a concrete managed-daemon settlement.
// It is the ordinary-install counterpart to recovery_claimed: provider restore
// is permitted only after one of those two durable facts exists. The transition
// is serialized with Install/recovery phase changes by adopted-entries.lock.
//
// Provider provenance created before provider-install-phase.json existed is a
// special compatibility case: an exact terminal stop settlement is itself new,
// positive evidence that managed ownership was settled. Only that caller reaches
// this function, so a physically absent marker may be bootstrapped directly to
// managed_settled. An existing corrupt/link/unreadable marker remains fail-closed.
func markProviderManagedSettled(manifestName string) error {
	return withAdoptedEntriesLock(func() error {
		phase, err := readProviderInstallPhase(manifestName)
		if err != nil {
			path, pathErr := providerInstallPhasePath(manifestName)
			if pathErr != nil {
				return pathErr
			}
			if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
				return err
			}
			return writeProviderInstallPhase(manifestName, providerInstallPhaseManagedSettled)
		}
		switch phase {
		case providerInstallPhaseManagedSettled:
			return nil
		case providerInstallPhaseStarted, providerInstallPhaseRecoveryClaimed:
			return writeProviderInstallPhase(manifestName, providerInstallPhaseManagedSettled)
		default:
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
	})
}

// bootstrapLegacyProviderManagedSettled migrates only the durable legacy
// de-adopt states which already assert that managed ownership was settled or
// removed. It never infers settlement from present-day absence alone. The
// caller's final supervisor ownership gate still runs before provider restore.
func bootstrapLegacyProviderManagedSettled(manifestName string) bool {
	bootstrapped := false
	err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		var current *AdoptProvenanceRecord
		for i := range store.Records {
			candidate := &store.Records[i]
			if candidate.ManifestName != manifestName {
				continue
			}
			if current != nil || candidate.ProviderSource == nil {
				return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
			}
			current = candidate
		}
		if current == nil || current.OperationState != AdoptOperationStateDeAdopting {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		phase := current.ProviderSource.DeAdoptPhase
		if phase != "managed_stop_settled" && phase != "managed_removed" {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		path, err := providerInstallPhasePath(manifestName)
		if err != nil {
			return err
		}
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		if err := writeProviderInstallPhase(manifestName, providerInstallPhaseManagedSettled); err != nil {
			return err
		}
		bootstrapped = true
		return nil
	})
	return err == nil && bootstrapped
}

// providerInstallPhaseAllowsRestore is the final restore gate. A genuine
// pre-Install recovery may arrive here with a still-not_started marker because
// older recovery sequencing advanced de-adopt before claiming the marker. The
// gate runs while recoverProviderActivation holds the supervisor-intent lock;
// atomically claiming not_started under adopted-entries.lock therefore closes
// the Install-vs-restore race: if Install already changed the marker to started,
// the claim loses and restore is refused; if recovery claims first, Install is
// permanently refused before it can mutate managed ownership. Legacy receipts
// with no marker are admitted only from a durable de_adopting settled/removed
// phase, never from current filesystem absence.
func providerInstallPhaseAllowsRestore(manifestName string) bool {
	phase, err := readProviderInstallPhase(manifestName)
	if err != nil {
		if !bootstrapLegacyProviderManagedSettled(manifestName) {
			return false
		}
		phase = providerInstallPhaseManagedSettled
	}
	if phase == providerInstallPhaseRecoveryClaimed || phase == providerInstallPhaseManagedSettled {
		return true
	}
	if phase != providerInstallPhaseNotStarted {
		return false
	}
	current, found, err := ReadAdoptProvenance(manifestName)
	if err != nil || !found || current.ProviderSource == nil {
		return false
	}
	claimed, err := providerInstallNeverStarted(current)
	return err == nil && claimed
}
