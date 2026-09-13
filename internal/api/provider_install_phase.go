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

func providerInstallPhaseAllowsRestore(manifestName string) bool {
	phase, err := readProviderInstallPhase(manifestName)
	if err != nil {
		if !bootstrapLegacyProviderManagedSettled(manifestName) {
			return false
		}
		phase = providerInstallPhaseManagedSettled
	}
	if phase == providerInstallPhaseRecoveryClaimed || phase == providerInstallPhaseManagedSettled {
		return providerRestoreBindingsClear(manifestName)
	}
	if phase != providerInstallPhaseNotStarted {
		return false
	}
	current, found, err := ReadAdoptProvenance(manifestName)
	if err != nil || !found || current.ProviderSource == nil {
		return false
	}
	claimed, err := providerInstallNeverStarted(current)
	return err == nil && claimed && providerRestoreBindingsClear(manifestName)
}
